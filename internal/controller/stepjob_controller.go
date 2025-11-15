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
	scheduledStartTimeAnnotation = "batch.step-job-operator.kubebuilder.io/scheduled-start-at"
	jobOwnerKey                  = ".metadata.controller"
)

func checkDuplicateStepNames(stepJob *stepjobv1.StepJob) bool {
	stepNames := make(map[string]bool)
	for _, step := range stepJob.Spec.Steps {
		if stepNames[*step.Name] {
			return true
		}
		stepNames[*step.Name] = true
	}
	return false
}

func constructJobForStep(namespace string, step *stepjobv1.Step, scheduledTime time.Time) (*kbatch.Job, error) {
	// We want job names for a given nominal start time to have a deterministic name to avoid the same job being created twice
	name := fmt.Sprintf("%s-%d", step.Name, scheduledTime.Unix())

	job := &kbatch.Job{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      make(map[string]string),
			Annotations: make(map[string]string),
			Name:        name,
			Namespace:   stepJob.Namespace,
		},
		Spec: *stepJob.Spec.JobTemplate.Spec.DeepCopy(),
	}
	for k, v := range cronJob.Spec.JobTemplate.Annotations {
		job.Annotations[k] = v
	}
	job.Annotations[scheduledTimeAnnotation] = scheduledTime.Format(time.RFC3339)
	for k, v := range cronJob.Spec.JobTemplate.Labels {
		job.Labels[k] = v
	}
	if err := ctrl.SetControllerReference(cronJob, job, r.Scheme); err != nil {
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

func getStepByName(name string, stepJob *stepjobv1.StepJob) (*stepjobv1.Step, error) {
	for _, step := range stepJob.Spec.Steps {
		if *step.Name == name {
			return step, nil
		}
	}

	return nil, errors.New(fmt.Sprintf("%s step not found", name))
}

func getNextSchedule(stepJob *stepjobv1.StepJob, now time.Time) (lastMissed time.Time, next time.Time, err error) {
	// parse cron schedule on the stepjob
	sched, err := cron.ParseStandard(stepJob.Spec.Schedule)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("Unparseable schedule %q: %v", stepJob.Spec.Schedule, err)
	}

	// figure out the earliest time the job could run, either last scheduled time or creation time
	var earliestTime time.Time
	if stepJob.Status.LastScheduleTime != nil {
		earliestTime = stepJob.Status.LastScheduleTime.Time
	} else {
		earliestTime = stepJob.ObjectMeta.CreationTimestamp.Time
	}

	// conside the deadline for jobs that are missed for any reason
	if stepJob.Spec.StartingDeadlineSeconds != nil {
		schedulingDeadline := now.Add(-time.Second * time.Duration(*stepJob.Spec.StartingDeadlineSeconds))

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

func isJobFinished(job *kbatch.Job) (bool, kbatch.JobConditionType) {
	for _, c := range job.Status.Conditions {
		if (c.Type == kbatch.JobComplete || c.Type == kbatch.JobFailed) && c.Status == corev1.ConditionTrue {
			return true, c.Type
		}
	}

	return false, ""
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
	var stepJob *stepjobv1.StepJob
	if err := r.Get(ctx, req.NamespacedName, stepJob); err != nil {
		logger.Error(err, "unable to fetch StepJob", req.NamespacedName)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2a: ensure unique step names - TODO: move to a validating webhook
	if duplicateStepName := checkDuplicateStepNames(stepJob); duplicateStepName == true {
		return ctrl.Result{}, fmt.Errorf("duplicate step names found in StepJob %s", stepJob.Name)
	}

	// 2b: list all active jobs, and update the status
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
		_, finishedType := isJobFinished(&job)
		switch finishedType {
		case "": // ongoing
			activeJobs = append(activeJobs, &allChildJobs.Items[i])
		case kbatch.JobFailed:
			failedJobs = append(failedJobs, &allChildJobs.Items[i])
		case kbatch.JobComplete:
			successfulJobs = append(successfulJobs, &allChildJobs.Items[i])
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
	// TODO: still need to consider which job to run, also to update the statuses

	if mostRecentTime != nil {
		stepJob.Status.LastScheduleTime = &metav1.Time{Time: *mostRecentTime}
	} else {
		stepJob.Status.LastScheduleTime = nil
	}

	// 2c: determine current step
	if stepJob.Status.CurrentStep == nil {
		currentStepName := stepJob.Spec.StartAt
		var err error
		stepJob.Status.CurrentStep, err = getStepByName(currentStepName, stepJob)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("starting step %s not found", currentStepName)
		}
	}

	// 3. check if the step job is suspended
	if stepJob.Spec.Suspend != nil && *stepJob.Spec.Suspend {
		logger.Info("stepjob suspended, skipping")
		return ctrl.Result{}, nil
	}

	// 4. custom logic for determining what the current step should be based on state of cluster and stepjob order/conditions

	// 5. determine the timing now for running the current step

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

	// attempt to create the missed job
	job, err := constructJobForStep(stepJob.GetNamespace(), stepJob.Status.CurrentStep, missedRun)
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

	// launch current step if it hasn't run yet (depending on condition)

	// 5. clean up old step jobs if either successful/unsuccessful and update the status

	// figure out the next times that we need to create
	// jobs at (or anything we missed).
	missedRun, nextRun, err := getNextSchedule(&cronJob, r.Now())
	if err != nil {
		log.Error(err, "unable to figure out CronJob schedule")
		// we don't really care about requeuing until we get an update that
		// fixes the schedule, so don't return an error
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *StepJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&stepjobv1.StepJob{}).
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
