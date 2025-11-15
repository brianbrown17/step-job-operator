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

package v1

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition describes how what information to check for the step.
// +kubebuilder:validation:Enum=Allow;Forbid;Replace
type Condition string

const (
	ExitCode Condition = "ExitCode"
	Stdout   Condition = "Stdout"
	Stderr   Condition = "Stderr"
)

// A toleration operator is the set of operators that can be used in a job condition.
// +kubebuilder:validation:Enum=Allow;Forbid;Replace
type Operator string

const (
	Exists Operator = "Exists"
	Equal  Operator = "Equal"
)

type JobCondition struct {
	// Condition specifies the condition to be checked for the step.
	Condition Condition `json:"condition,omitempty"`

	// Operator specifies the operator to be used for comparison.
	Operator Operator `json:"operator,omitempty"`

	// Value specifies the value to be compared with.
	Value *string `json:"value,omitempty"`
}

type NextStep struct {
	// +kubebuilder:validation:Required
	Name *string `json:"name"`

	JobCondition *JobCondition `json:"condition,omitempty"`
}

type Step struct {
	// +kubebuilder:validation:Required
	// The name of the step
	Name *string `json:"name"`

	// Specifies the job that will be created when executing a CronJob.
	JobTemplate batchv1.JobTemplateSpec `json:"jobTemplate"`

	// NextStep specifies the next steps to be executed based on the condition.
	NextStep []NextStep `json:"next,omitempty"`
}

// StepJobSpec defines the desired state of StepJob
type StepJobSpec struct {
	// +kubebuilder:validation:MinLength=0
	// The schedule in Cron format, see https://en.wikipedia.org/wiki/Cron.
	Schedule string `json:"schedule"`

	// The name of the job(s) to start at
	StartAt string `json:"startAt"`

	// +kubebuilder:validation:UniqueItems=true
	// the steps comprising the workflow, including the job templates
	Steps []*Step `json:"steps"`

	// +kubebuilder:validation:Minimum=0
	// Optional deadline in seconds for starting the job if it misses scheduled
	// time for any reason.  Missed jobs executions will be counted as failed ones.
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds"`

	// This flag tells the controller to suspend subsequent executions, it does
	// not apply to already started executions.  Defaults to false.
	Suspend *bool `json:"suspend"`

	// +kubebuilder:validation:Minimum=0
	// The number of successful finished jobs to retain.
	// This is a pointer to distinguish between explicit zero and not specified.
	SuccessfulJobsHistoryLimit int32 `json:"successfulJobsHistoryLimit"`

	// +kubebuilder:validation:Minimum=0
	// The number of failed finished jobs to retain.
	// This is a pointer to distinguish between explicit zero and not specified.
	FailedJobsHistoryLimit int32 `json:"failedJobsHistoryLimit"`
}

// StepJobStatus defines the observed state of StepJob
type StepJobStatus struct {
	// Current step to manage.
	// +optional
	CurrentStep *Step `json:"nextStep,omitempty"`

	// Active running jobs
	// +optional
	Active []corev1.ObjectReference `json:"active,omitempty"`

	// Completed jobs
	// +optional
	Completed []corev1.ObjectReference `json:"completed,omitempty"`

	// Information when was the last time the job was successfully scheduled.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// StepJob is the Schema for the stepjobs API
type StepJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StepJobSpec   `json:"spec,omitempty"`
	Status StepJobStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StepJobList contains a list of StepJob
type StepJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StepJob `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StepJob{}, &StepJobList{})
}

func StrPtr(str string) *string {
	return &str
}
