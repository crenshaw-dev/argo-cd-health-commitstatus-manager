package main

import (
	"context"
	"fmt"
	"github.com/argoproj/argo-cd/v2/pkg/client/clientset/versioned"
	"github.com/argoproj/argo-cd/v2/pkg/client/informers/externalversions"
	listers "github.com/argoproj/argo-cd/v2/pkg/client/listers/application/v1alpha1"
	"github.com/argoproj/gitops-engine/pkg/health"
	promoter_v1alpha1 "github.com/zachaller/promoter/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	"net/url"
	"os"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"time"
)

func main() {
	klog.InitFlags(nil)
	ctrl.SetLogger(zap.New(zap.WriteTo(os.Stdout)))
	ctx := signals.SetupSignalHandler()

	kubeconfig := ctrl.GetConfigOrDie()
	c, err := versioned.NewForConfig(kubeconfig)
	if err != nil {
		panic(err)
	}
	factory := externalversions.NewSharedInformerFactory(c, 1*time.Hour)
	appInformer := factory.Argoproj().V1alpha1().Applications().Informer()
	appLister := factory.Argoproj().V1alpha1().Applications().Lister()

	// Create a lister based on the informer

	_, err = appInformer.AddEventHandler(
		&cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				updateCommitStatuses(appLister)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				updateCommitStatuses(appLister)
			},
			DeleteFunc: func(obj interface{}) {
				updateCommitStatuses(appLister)
			},
		},
	)

	factory.Start(ctx.Done())

	select {}
}

func updateCommitStatuses(appLister listers.ApplicationLister) {
	scheme := runtime.NewScheme()
	utilruntime.Must(promoter_v1alpha1.AddToScheme(scheme))
	kubeconfig := ctrl.GetConfigOrDie()
	kubeClient, err := client.New(kubeconfig, client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}

	apps, err := appLister.List(labels.NewSelector())
	if err != nil {
		panic(err)
	}
	for _, app := range apps {
		if app.Spec.SourceHydrator == nil {
			continue
		}

		repoURL, err := url.Parse(app.Spec.SourceHydrator.DrySource.RepoURL)
		if err != nil {
			panic(err)
		}

		// Get owner and repo name from the GitHub URL
		// FIXME: support more than just GitHub
		owner := repoURL.Path[1 : strings.Index(repoURL.Path[1:], "/")+1]
		repo := repoURL.Path[strings.LastIndex(repoURL.Path, "/")+1:]

		state := promoter_v1alpha1.CommitStatusPending
		if app.Status.Health.Status == health.HealthStatusHealthy {
			state = promoter_v1alpha1.CommitStatusSuccess
		} else if app.Status.Health.Status == health.HealthStatusDegraded {
			state = promoter_v1alpha1.CommitStatusFailure
		}

		commitStatusName := app.Name + "-health"

		desiredCommitStatus := promoter_v1alpha1.CommitStatus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      commitStatusName,
				Namespace: "default",
			},
			Spec: promoter_v1alpha1.CommitStatusSpec{
				RepositoryReference: &promoter_v1alpha1.Repository{
					Owner: owner,
					Name:  repo,
					ScmProviderRef: promoter_v1alpha1.NamespacedObjectReference{
						Name:      "scmprovider-example",
						Namespace: "default",
					},
				},
				Sha:         app.Status.Sync.Revision,
				Name:        commitStatusName,
				Description: fmt.Sprintf("App %s is %s", app.Name, state),
				State:       state,
				Url:         "https://example.com",
			},
		}

		currentCommitStatus := promoter_v1alpha1.CommitStatus{}
		err = kubeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: commitStatusName}, &currentCommitStatus)
		if err != nil {
			if client.IgnoreNotFound(err) != nil {
				panic(err)
			}
			err = kubeClient.Create(context.Background(), &desiredCommitStatus)
			if err != nil {
				panic(err)
			}
			continue
		}

		currentCommitStatus.Spec = desiredCommitStatus.Spec
		err = kubeClient.Update(context.Background(), &currentCommitStatus)
		if err != nil {
			panic(err)
		}
	}
}
