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
	"fmt"
	"time"

	klog "sigs.k8s.io/controller-runtime/pkg/log"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	testsv1alpha1 "github.com/zbsss/integration-test-operator/api/v1alpha1"
)

// TestRunReconciler reconciles a TestRun object
type TestRunReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const finalizerName = "testruns.finalizer.tests.mkurleto.io"

//+kubebuilder:rbac:groups=tests.mkurleto.io,resources=testruns;tests,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=tests.mkurleto.io,resources=testruns/status;tests/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=tests.mkurleto.io,resources=testruns/finalizers;tests/finalizers,verbs=update
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the TestRun object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.16.3/pkg/reconcile
func (r *TestRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := klog.FromContext(ctx)

	log.Info("Reconciling TestRun")

	// Fetch the TestRun instance
	testRun := &testsv1alpha1.TestRun{}
	if err := r.Get(ctx, req.NamespacedName, testRun); err != nil {
		if errors.IsNotFound(err) {
			log.Info("TestRun resource not found. Assuming it was deleted.")
			return ctrl.Result{}, nil
		}
		// Log and return all other errors.
		log.Error(err, "unable to fetch TestRun")
		return ctrl.Result{}, err
	}

	log.Info("Fetched TestRun", "UID", testRun.UID, "Phase", testRun.Status.Phase)

	// Check if the TestRun is marked for deletion
	if testRun.DeletionTimestamp != nil {
		// Pre-deletion logic here
		// ...
		log.Info("TestRun marked for deletion", "UID", testRun.UID, "Phase", testRun.Status.Phase)

		// Remove finalizer after handling deletion
		controllerutil.RemoveFinalizer(testRun, finalizerName)
		err := r.Update(ctx, testRun)
		if err != nil {
			return reconcile.Result{}, err
		}

		log.Info("Finalizer removed", "UID", testRun.UID, "Phase", testRun.Status.Phase)

		// Return and do not requeue
		return reconcile.Result{}, nil
	}

	// Add or update finalizer
	if !controllerutil.ContainsFinalizer(testRun, finalizerName) {
		controllerutil.AddFinalizer(testRun, finalizerName)
		err := r.Update(ctx, testRun)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	// Fetch the Test instance
	test := &testsv1alpha1.Test{}
	err := r.Get(ctx,
		types.NamespacedName{
			Name:      testRun.Spec.TestRef.Name,
			Namespace: testRun.Spec.TestRef.Namespace,
		}, test)
	if err != nil {
		log.Error(err, "unable to fetch Test")
		return ctrl.Result{}, err
	}

	ls := map[string]string{
		"testrun":    testRun.Name,
		"testrunUID": string(testRun.UID),
	}

	// Define the job the TestRun should run
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-", test.Name),
			Namespace:    test.Namespace,
			Labels:       ls,
		},
		Spec: test.Spec.JobTemplate,
	}

	// Set the TestRun instance as the owner and controller of the Job
	// This helps in garbage collection of Jobs when the TestRun is deleted
	if err := controllerutil.SetControllerReference(testRun, job, r.Scheme); err != nil {
		return reconcile.Result{}, err
	}

	// Check if the Job already exists
	labelSelector := labels.SelectorFromSet(ls)
	existingJobs := &batchv1.JobList{}
	if err := r.List(ctx, existingJobs, &client.ListOptions{Namespace: test.Namespace, LabelSelector: labelSelector}); err != nil {
		log.Error(err, "unable to list jobs for TestRun", "TestRun.Namespace", test.Namespace, "TestRun.Name", testRun.Name)
		return reconcile.Result{}, err
	}

	// Check if we already have a job for this TestRun, if not create a new one
	if len(existingJobs.Items) == 0 {
		log.Info("Creating a new Job", "Job.Namespace", job.Namespace)

		testRun.Status.Phase = testsv1alpha1.TestRunPhaseStarting
		if err := r.Status().Update(ctx, testRun); err != nil {
			log.Error(err, "failed to update TestRun status")
			return reconcile.Result{}, err
		}

		if err := r.Create(ctx, job); err != nil {
			log.Error(err, "failed to create Job")
			return reconcile.Result{}, err
		}
		// Job created successfully - possibly requeue to check job status later
		return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
	} else {
		job := existingJobs.Items[0] // Assuming single job per TestRun for simplicity
		switch {
		case job.Status.Active > 0:
			testRun.Status.Phase = testsv1alpha1.TestRunPhaseRunning
			if err := r.Status().Update(ctx, testRun); err != nil {
				log.Error(err, "failed to update TestRun status")
				return ctrl.Result{}, err
			}
			// Requeue the request to check the job status after a specified delay
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		case job.Status.Succeeded > 0:
			testRun.Status.Phase = testsv1alpha1.TestRunPhaseCompleted
			testRun.Status.Message = "Job completed successfully"
		case job.Status.Failed > 0:
			testRun.Status.Phase = testsv1alpha1.TestRunPhaseFailed
			testRun.Status.Message = "Job failed"
		}

		// Always update the TestRun status
		if err := r.Status().Update(ctx, testRun); err != nil {
			log.Error(err, "failed to update TestRun status")
			return reconcile.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *TestRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&testsv1alpha1.TestRun{}).
		Complete(r)
}
