/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/robfig/cron"
	kbatch "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	stepjobv1 "step-job-operator.kubebuilder.io/project/api/v1"
)

// StepJobReconciler reconciles a StepJob object
type StepJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Clock  clock.Clock
}

var (
	scheduledStartTimeAnnotation = "batch.step-job-operator.kubebuilder.io/scheduled-start-at" // TODO: normalize these names
	jobOwnerKey                  = ".metadata.controller"
	stepNameLabel                = "stepjob/step-name"
)

func checkDuplicateStepNames(stepJob *stepjobv1.StepJob) bool {
	stepNames := make(map[string]string)
	for _, step := range stepJob.Spec.Steps {
		if _, exists := stepNames[step.Name]; exists {
			return true
		}
		stepNames[step.Name] = ""
	}
	return false
}

func constructJobForStep(stepJob *stepjobv1.StepJob, step *stepjobv1.Step, scheduledTime time.Time, scheme *runtime.Scheme) (*kbatch.Job, error) {
	// We want job names for a given nominal start time to have a deterministic name to avoid the same job being created twice
	name := fmt.Sprintf("%s-%d", step.Name, scheduledTime.Unix())

	job := &kbatch.Job{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      make(map[string]string),
			Annotations: make(map[string]string),
			Name:        name,
			Namespace:   stepJob.Namespace,
		},
		Spec: *step.JobTemplate.Spec.DeepCopy(),
	}
	for k, v := range step.JobTemplate.Annotations {
		job.Annotations[k] = v
	}
	job.Annotations[scheduledStartTimeAnnotation] = scheduledTime.Format(time.RFC3339)
	for k, v := range step.JobTemplate.Labels {
		job.Labels[k] = v
	}
	// Add the step name label so we can identify which step this job belongs to
	job.Labels[stepNameLabel] = step.Name
	if err := ctrl.SetControllerReference(stepJob, job, scheme); err != nil {
		return nil, err
	}

	return job, nil
}

func getScheduledStartTimeForJob(job *kbatch.Job) (*time.Time, error) {
	timeRaw := job.Annotations[scheduledStartTimeAnnotation]
	if len(timeRaw) == 0 {
		return nil, nil
	}

	timeParsed, err := time.Parse(time.RFC3339, timeRaw)
	if err != nil {
		return nil, err
	}

	return &timeParsed, nil
}

// TODO: move StepJob method function to the api/ package and make them methods on the StepJob struct instead of passing the StepJob as an argument

// getStepByName returns pointer to a step with a given name, or error if no step with the name exists
func (sj *stepjobv1.StepJob) getStepByName(name string) (*stepjobv1.Step, error) {
	for _, step := range sj.Spec.Steps {
		if step.Name == name {
			return &step, nil
		}
	}

	return nil, errors.New(fmt.Sprintf("%s step not found", name))
}

// getNextSchedule determines the next time a step job should run based on the cron schedule and the last time it was scheduled to run.
// It also returns the most recent missed schedule time if the current time is after the next scheduled time,
// while considering the starting deadline for missed jobs
// (i.e. if the most recent missed schedule time is before the current time minus the starting deadline, it will return a zero time for the missed schedule time).
func (sj *stepjobv1.StepJob) getNextScheduleTime(now time.Time) (lastMissed time.Time, next time.Time, err error) {
	// parse cron schedule on the stepjob
	sched, err := cron.ParseStandard(sj.Spec.Schedule)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("Unparseable schedule %q: %v", sj.Spec.Schedule, err)
	}

	// figure out the earliest time the job could run, either last scheduled time or creation time
	var earliestTime time.Time
	if sj.Status.LastScheduleTime != nil {
		earliestTime = sj.Status.LastScheduleTime.Time
	} else {
		earliestTime = sj.ObjectMeta.CreationTimestamp.Time
	}

	// conside the deadline for jobs that are missed for any reason
	if sj.Spec.StartingDeadlineSeconds != nil {
		schedulingDeadline := now.Add(-time.Second * time.Duration(*sj.Spec.StartingDeadlineSeconds))

		if schedulingDeadline.After(earliestTime) {
			earliestTime = schedulingDeadline
		}
	}
	// if the earlist time for missed jobs is after the current time, don't report any other missed jobs
	if earliestTime.After(now) {
		return time.Time{}, sched.Next(now), nil
	} else { // otherwise get the most recent missed time
		for t := sched.Next(earliestTime); !t.After(now); t = sched.Next(t) {
			lastMissed = t
		}
	}
	return lastMissed, sched.Next(now), nil
}

// chooseNextStep determines the next step to run based on the current step and whether the current step succeeded or not
func chooseNextStep(current *stepjobv1.Step, succeeded bool) *string {
	var step *string

	for _, n := range current.NextStep {
		if n.JobCondition == nil {
			step = &n.Name
			continue
		}

		// for now: success/failure routing
		if n.JobCondition.Condition == stepjobv1.ExitCode {
			if succeeded && n.JobCondition.Operator == stepjobv1.Equal {
				return &n.Name
			}
		}
	}
	return step
}

type StepAction string // the action to take for a given step based on the state of the cluster and the step job
const (
	StepActionRun    StepAction = "Run"    // create a Job for this step
	StepActionWait   StepAction = "Wait"   // a Job is currently running
	StepActionFinish StepAction = "Finish" // workflow execution complete
)

// calculateCurrentStep determines what the current step is based on the state of the cluster,
// and the step job, and also determines what action to take for the current step (run/wait/finish)
func (sj *stepjobv1.StepJob) calculateCurrentStep(childJobs []*kbatch.Job) (*stepjobv1.Step, StepAction, error) {
	// if no child jobs, should begin at the starting step
	if len(childJobs) == 0 {
		start, err := sj.getStepByName(sj.Spec.StartAt)
		if err != nil {
			return nil, StepActionFinish, fmt.Errorf("start step %s not found", sj.Spec.StartAt)
		}
		return start, StepActionRun, nil
	}

	var lastFinishedJob *kbatch.Job
	var lastFinishedTime *time.Time

	for _, job := range childJobs {
		jobCondStatus := getJobCondStatus(job.Status.Conditions)
		if jobCondStatus == "" { // "" indicates job is still active
			stepName := getJobStepName(job)         // get the step name from the job's labels
			step, err := sj.getStepByName(stepName) // get the step object from the step name
			if err != nil {
				return nil, StepActionFinish, fmt.Errorf("unknown step %s", stepName)
			}
			return step, StepActionWait, nil
		} else if jobCondStatus == kbatch.JobComplete || jobCondStatus == kbatch.JobFailed {
			t := job.Status.CompletionTime.Time
			if lastFinishedTime == nil || t.After(*lastFinishedTime) {
				lastFinishedJob = job
				lastFinishedTime = &t
			}
		}
	}

	if lastFinishedJob == nil {
		return nil, StepActionFinish, nil
	}

	lastStepName := getJobStepName(lastFinishedJob)
	current, err := sj.getStepByName(lastStepName)
	if err != nil {
		return nil, StepActionFinish, fmt.Errorf("unknown step %s", lastStepName)
	}

	succeeded := isJobSucceeded(lastFinishedJob)
	nextName := chooseNextStep(current, succeeded)

	if nextName == nil || *nextName == "" {
		return nil, StepActionFinish, nil
	}

	next, err := sj.getStepByName(*nextName)
	if err != nil {
		return nil, StepActionFinish, fmt.Errorf("next step %s not found", *nextName)
	}

	return next, StepActionRun, nil
}

func isJobSucceeded(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func isJobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// getJobStepName returns the step name from a job's labels
func getJobStepName(job *kbatch.Job) string {
	return job.Labels[stepNameLabel]
}

// getJobCondStatus checks if a job is finished and if so what the condition of the job is (failed or complete)
// if the job is not finished, it returns an empty string
func getJobCondStatus(conditions []kbatch.JobCondition) kbatch.JobConditionType {
	for _, c := range conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		if c.Type == kbatch.JobComplete || c.Type == kbatch.JobFailed {
			return c.Type
		}
	}
	return ""
}

// +kubebuilder:rbac:groups=batch.step-job-operator.kubebuilder.io,resources=stepjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch.step-job-operator.kubebuilder.io,resources=stepjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch.step-job-operator.kubebuilder.io,resources=stepjobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get,update,patch
// +kubebuilder:rbac:groups=batch,resources=jobs/finalizers,verbs=update
func (r *StepJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1: load state of the StepJob, ignore not-found errors and don't requeue
	stepJob := &stepjobv1.StepJob{}
	if err := r.Get(ctx, req.NamespacedName, stepJob); err != nil {
		logger.Error(err, "unable to fetch StepJob", req.NamespacedName)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2a: ensure unique step names
	// TODO: move to a validating webhook
	if duplicateStepName := checkDuplicateStepNames(stepJob); duplicateStepName == true {
		return ctrl.Result{}, fmt.Errorf("duplicate step names found in StepJob %s", stepJob.Name)
	}

	// 2b: list all current jobs in the cluster owned by the StepJob, and update the status
	var allChildJobs kbatch.JobList
	if err := r.List(ctx, &allChildJobs, client.InNamespace(req.Namespace), client.MatchingFields{jobOwnerKey: req.Name}); err != nil {
		logger.Error(err, "unable to list child Jobs")
		return ctrl.Result{}, err
	}

	// find the active list of jobs
	var activeJobs []*kbatch.Job
	var successfulJobs []*kbatch.Job
	var failedJobs []*kbatch.Job
	var mostRecentTime *time.Time // most recent time a child job ran that is managed by this step job

	// categorize child jobs into active/failed/sucessful and determine most recent time
	for i, job := range allChildJobs.Items {

		switch getJobCondStatus(job.Status.Conditions) {
		case kbatch.JobComplete:
			successfulJobs = append(successfulJobs, &allChildJobs.Items[i])
		case kbatch.JobFailed:
			failedJobs = append(failedJobs, &allChildJobs.Items[i])
		case "": // ongoing
			activeJobs = append(activeJobs, &allChildJobs.Items[i])
		}

		// Get the scheduled start time for the step job from annotation
		scheduledStartTimeForJob, err := getScheduledStartTimeForJob(&job)
		if err != nil {
			logger.Error(err, "unable to parse schedule time for child job", "job", &job)
			continue
		}
		// If not nil, compare with mostRecentTime and update if it is more recent
		if scheduledStartTimeForJob != nil {
			if mostRecentTime != nil || scheduledStartTimeForJob.After(*mostRecentTime) {
				mostRecentTime = scheduledStartTimeForJob
			}
		}
	}

	if mostRecentTime != nil {
		stepJob.Status.LastScheduleTime = &metav1.Time{Time: *mostRecentTime}
	} else {
		stepJob.Status.LastScheduleTime = nil
	}

	// Set status with current step if a job is active, otherwise set to nil
	if stepJob.Status.CurrentStep == nil {
		currentStepName := stepJob.Spec.StartAt
		currentStep, err := getStepByName(currentStepName, stepJob)
		if err != nil {
			logger.Info("starting step not found", "step", currentStepName)
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		stepJob.Status.CurrentStep = stepjobv1.StrPtr(currentStep.Name)
	}

	// 3. check if the step job is suspended
	if stepJob.Spec.Suspend != nil && *stepJob.Spec.Suspend {
		logger.Info("stepjob suspended, skipping")
		return ctrl.Result{}, nil
	}

	// 4. determine the current step and whether to run/wait/finish for the current step based on the state of the cluster
	currentStep, stepAction, err := stepJob.calculateCurrentStep(activeJobs)
	if err != nil {
		logger.Error(err, "unable to calculate current step")
		return ctrl.Result{}, err
	}
	stepJob.Status.CurrentStep = stepjobv1.StrPtr(currentStep.Name)

	// if the action is to wait, then we don't need to do anything else until the next reconcile when we will check if the job has finished or not
	if stepAction == StepActionWait {
		logger.Info("current step is still running, waiting for next reconcile", "step", currentStep.Name)
		return ctrl.Result{}, nil
	}

	if stepAction == StepActionFinish { // TODO: indiicate complete status in the StepJob status conditions (same pattern as Job conditions), and set a completion time, don't requeue
		logger.Info("workflow execution complete")
		return ctrl.Result{}, nil
	}

	// this all needs to be for the currentStep - there needs to logic before to determine what the current step is, namely is already running, if successful

	// get the current time
	now := r.Clock.Now()

	// determine if the current time is after the cron schedule set in the step job
	missedRun, nextRun, err := getNextSchedule(stepJob, now)
	if err != nil {
		logger.Error(err, "unable to determine next schedule time")
		return ctrl.Result{}, err
	}

	// if the current time is after the next scheduled run time, we need to run the job
	if missedRun.IsZero() {
		logger.Info("no upcoming scheduled times, waiting until next run", "next run", nextRun)
		return ctrl.Result{RequeueAfter: nextRun.Sub(now)}, nil
	}

	// get the current step object
	currentStep, err := sj.getStepByName(*stepJob.Status.CurrentStep, stepJob)
	if err != nil {
		logger.Error(err, "unable to find current step")
		return ctrl.Result{}, err
	}

	// attempt to create the missed job
	job, err := constructJobForStep(stepJob, currentStep, missedRun, r.Scheme)
	if err != nil {
		logger.Error(err, "unable to construct job from step template")
		// reque after the next scheduled time
		return ctrl.Result{RequeueAfter: nextRun.Sub(now)}, nil
	}

	// and create it on the cluster
	if err := r.Create(ctx, job); err != nil {
		logger.Error(err, "unable to create Job for step", "job", job)
		return ctrl.Result{}, err
	}

	logger.Info("created Job for missed run", "job", job.Name, "scheduled time", missedRun)

	// launch current step if it hasn't run yet (depending on condition)
	// otherwise requeue after the next scheduled time (if the job updates the status to trigger the next step, it will requeue immediately)

	// TODO: clean up old jobs based on history limits

	// TODO: calcualte the status here

	// requeue after the next scheduled time
	return ctrl.Result{RequeueAfter: nextRun.Sub(now)}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *StepJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Set up an index for the job owner key
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &kbatch.Job{}, jobOwnerKey, func(rawObj client.Object) []string {
		// grab the job object, extract the owner
		job := rawObj.(*kbatch.Job)
		owner := metav1.GetControllerOf(job)
		if owner == nil {
			return nil
		}
		// make sure it's a StepJob
		if owner.APIVersion != stepjobv1.GroupVersion.String() || owner.Kind != "StepJob" {
			return nil
		}

		// and return it
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&stepjobv1.StepJob{}).
		Owns(&kbatch.Job{}).
		Complete(r)
}

// high level overview:
// get the step job
// if you can't find the step job, ignore the error and don't requeue
// list all jobs controlled by step job
// categorize jobs into active, successful, failed (active if job is ongoing)
// find the most recent step job execution out of jobs (or creation timestamp if this does exist)
// update the status of the step job with the most recent execution time
// Just figure out the next time it should run, don't worry about making up for previous funs
