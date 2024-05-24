package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v2/pkg/client/clientset/versioned"
	"github.com/argoproj/argo-cd/v2/pkg/client/informers/externalversions"
	listers "github.com/argoproj/argo-cd/v2/pkg/client/listers/application/v1alpha1"
	"github.com/argoproj/gitops-engine/pkg/health"
	"github.com/cespare/xxhash/v2"
	promoter_v1alpha1 "github.com/zachaller/promoter/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
)

const CommitStatusAnnotation = "promoter.argoproj.io/commit-status"

type AggregateStuff struct {
	app     *v1alpha1.Application
	status  *promoter_v1alpha1.CommitStatus
	changed bool
}

type objKey struct {
	repo     string
	revision string
}

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

	aggregates := map[objKey][]*AggregateStuff{}

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

		stuff := &AggregateStuff{
			app: app,
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

		commitStatusName := app.Name + "/health"
		resourceName := strings.ReplaceAll(commitStatusName, "/", "-")

		desiredCommitStatus := promoter_v1alpha1.CommitStatus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      resourceName,
				Namespace: "default",
				Labels: map[string]string{
					CommitStatusAnnotation: "app-healthy",
				},
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
		err = kubeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: resourceName}, &currentCommitStatus)
		if err != nil {
			if client.IgnoreNotFound(err) != nil {
				panic(err)
			}
			// Create
			err = kubeClient.Create(context.Background(), &desiredCommitStatus)
			if err != nil {
				panic(err)
			}
			currentCommitStatus = desiredCommitStatus
		} else {
			// Update
			currentCommitStatus.Spec = desiredCommitStatus.Spec
			err = kubeClient.Update(context.Background(), &currentCommitStatus)
			if err != nil {
				panic(err)
			}
		}

		stuff.status = &currentCommitStatus
		stuff.changed = true // Should check if there is a no-op

		key := objKey{
			repo:     app.Spec.SourceHydrator.GetApplicationSource().RepoURL,
			revision: app.Spec.SourceHydrator.GetApplicationSource().TargetRevision,
		}
		aggregates[key] = append(aggregates[key], stuff)
	}

	// Creates aggregated status
	for key, stuff := range aggregates {
		update := false
		for _, v := range stuff {
			update = update || v.changed
			if update {
				break
			}
		}
		if !update {
			continue
		}

		repo, revision := key.repo, key.revision
		resolveShaCmd := exec.Command("git", "ls-remote", repo, revision)
		out, err := resolveShaCmd.CombinedOutput()
		if err != nil {
			panic(err)
		}
		resolvedSha := strings.Split(string(out), "\t")[0]

		desc := ""
		resolvedState := promoter_v1alpha1.CommitStatusPending
		pending := 0
		healthy := 0
		degraded := 0
		for _, s := range stuff {
			if s.status.Spec.Sha != resolvedSha {
				pending++
			} else if s.status.Spec.State == promoter_v1alpha1.CommitStatusSuccess {
				healthy++
			} else if s.status.Spec.State == promoter_v1alpha1.CommitStatusFailure {
				degraded++
			} else {
				pending++
			}
		}

		//Resolve state
		if healthy == len(stuff) {
			resolvedState = promoter_v1alpha1.CommitStatusSuccess
			desc = fmt.Sprintf("%d/%d apps are healthy", healthy, len(stuff))
		} else if degraded == len(stuff) {
			resolvedState = promoter_v1alpha1.CommitStatusFailure
			desc = fmt.Sprintf("%d/%d apps are degraded", healthy, len(stuff))
		} else {
			desc = fmt.Sprintf("Waiting for apps to be healthy (%d/%d healthy, %d/%d degraded, %d/%d pending)", healthy, len(stuff), degraded, len(stuff), pending, len(stuff))
		}

		err = updateAggregatedStatus(kubeClient, revision, repo, resolvedSha, resolvedState, desc)
		if err != nil {
			panic(err)
		}

	}
}

func hash(data []byte) string {
	return strconv.FormatUint(xxhash.Sum64(data), 8)
}

func updateAggregatedStatus(kubeClient client.Client, revision string, repo string, sha string, state promoter_v1alpha1.CommitStatusState, desc string) error {

	repoURL, err := url.Parse(repo)
	if err != nil {
		panic(err)
	}
	owner := repoURL.Path[1 : strings.Index(repoURL.Path[1:], "/")+1]
	repository := repoURL.Path[strings.LastIndex(repoURL.Path, "/")+1:]

	commitStatusName := revision + "/health"
	resourceName := strings.ReplaceAll(commitStatusName, "/", "-") + "-" + hash([]byte(repo))

	desiredCommitStatus := promoter_v1alpha1.CommitStatus{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName,
			Namespace: "default",
			Labels: map[string]string{
				CommitStatusAnnotation: "healthy",
			},
		},
		Spec: promoter_v1alpha1.CommitStatusSpec{
			RepositoryReference: &promoter_v1alpha1.Repository{
				Owner: owner,
				Name:  repository,
				ScmProviderRef: promoter_v1alpha1.NamespacedObjectReference{
					Name:      "scmprovider-example",
					Namespace: "default",
				},
			},
			Sha:         sha,
			Name:        commitStatusName,
			Description: desc,
			State:       state,
			Url:         "https://example.com",
		},
	}

	currentCommitStatus := promoter_v1alpha1.CommitStatus{}
	err = kubeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: resourceName}, &currentCommitStatus)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			panic(err)
		}
		// Create
		err = kubeClient.Create(context.Background(), &desiredCommitStatus)
		if err != nil {
			panic(err)
		}
		currentCommitStatus = desiredCommitStatus
	} else {
		// Update
		currentCommitStatus.Spec = desiredCommitStatus.Spec
		err = kubeClient.Update(context.Background(), &currentCommitStatus)
		if err != nil {
			panic(err)
		}
	}

	return nil
}
