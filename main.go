package main

import (
	"context"
	"fmt"
	"github.com/cespare/xxhash/v2"
	"k8s.io/klog/v2"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	promoter_v1alpha1 "github.com/argoproj-labs/gitops-promoter/api/v1alpha1"
	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v2/pkg/client/clientset/versioned"
	"github.com/argoproj/argo-cd/v2/pkg/client/informers/externalversions"
	listers "github.com/argoproj/argo-cd/v2/pkg/client/listers/application/v1alpha1"
	"github.com/argoproj/gitops-engine/pkg/health"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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
		repo := strings.TrimRight(repoURL.Path[strings.LastIndex(repoURL.Path, "/")+1:], ".git")

		state := promoter_v1alpha1.CommitPhasePending
		if app.Status.Health.Status == health.HealthStatusHealthy && app.Status.Sync.Status == v1alpha1.SyncStatusCodeSynced {
			state = promoter_v1alpha1.CommitPhaseSuccess
		} else if app.Status.Health.Status == health.HealthStatusDegraded {
			state = promoter_v1alpha1.CommitPhaseFailure
		}

		commitStatusName := app.Name + "/health"
		resourceName := strings.ReplaceAll(commitStatusName, "/", "-")

		if strings.Contains(app.Name, "production-use2") {
			// For HCAP demo
			hcapCommitStatus := promoter_v1alpha1.CommitStatus{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hcap",
					Namespace: "argocd",
					Labels: map[string]string{
						CommitStatusAnnotation: "hcap",
					},
					Annotations: map[string]string{
						"hcapActive": "true",
					},
				},
				Spec: promoter_v1alpha1.CommitStatusSpec{
					RepositoryReference: &promoter_v1alpha1.Repository{
						Owner: owner,
						Name:  repo,
						ScmProviderRef: promoter_v1alpha1.NamespacedObjectReference{
							Name:      "scmprovider-sample",
							Namespace: "default",
						},
					},
					Sha:         app.Status.SourceHydrator.LastSuccessfulOperation.HydratedSHA,
					Name:        "production/hcap",
					Description: fmt.Sprintf("HCAP check for production"),
					//Phase:       "pending",
					Url: "https://example.com",
				},
			}
			currentCommitStatus := promoter_v1alpha1.CommitStatus{}
			err = kubeClient.Get(context.Background(), client.ObjectKey{Namespace: "argocd", Name: "hcap"}, &currentCommitStatus)
			if err != nil {
				if client.IgnoreNotFound(err) != nil {
					panic(err)
				}
				// Create
				hcapCommitStatus.Spec.Phase = "pending"
				err = kubeClient.Create(context.Background(), &hcapCommitStatus)
				if err != nil {
					panic(err)
				}
				currentCommitStatus = hcapCommitStatus
			} else {
				if currentCommitStatus.Annotations["hcapActive"] == "false" {
					currentCommitStatus.Spec.Phase = promoter_v1alpha1.CommitPhaseSuccess
				} else {
					currentCommitStatus.Spec.Phase = promoter_v1alpha1.CommitPhasePending
				}
				currentPhase := currentCommitStatus.Spec.Phase
				currentCommitStatus.Spec = hcapCommitStatus.Spec
				currentCommitStatus.Spec.Phase = currentPhase
				err = kubeClient.Update(context.Background(), &currentCommitStatus)
				if err != nil {
					panic(err)
				}
			}
		}

		desiredCommitStatus := promoter_v1alpha1.CommitStatus{
			ObjectMeta: metav1.ObjectMeta{
				Name:      resourceName,
				Namespace: "argocd",
				Labels: map[string]string{
					CommitStatusAnnotation: "app-healthy",
				},
			},
			Spec: promoter_v1alpha1.CommitStatusSpec{
				RepositoryReference: &promoter_v1alpha1.Repository{
					Owner: owner,
					Name:  repo,
					ScmProviderRef: promoter_v1alpha1.NamespacedObjectReference{
						Name:      "scmprovider-sample",
						Namespace: "default",
					},
				},
				Sha:         app.Status.Sync.Revision,
				Name:        commitStatusName,
				Description: fmt.Sprintf("App %s is %s", app.Name, state),
				Phase:       state,
				Url:         "https://example.com",
			},
		}

		currentCommitStatus := promoter_v1alpha1.CommitStatus{}
		err = kubeClient.Get(context.Background(), client.ObjectKey{Namespace: "argocd", Name: resourceName}, &currentCommitStatus)
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
			repo:     strings.TrimRight(app.Spec.SourceHydrator.GetSyncSource().RepoURL, ".git"),
			revision: app.Spec.SourceHydrator.GetSyncSource().TargetRevision,
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
		resolvedState := promoter_v1alpha1.CommitPhasePending
		pending := 0
		healthy := 0
		degraded := 0
		for _, s := range stuff {
			if s.status.Spec.Sha != resolvedSha {
				pending++
			} else if s.status.Spec.Phase == promoter_v1alpha1.CommitPhaseSuccess {
				healthy++
			} else if s.status.Spec.Phase == promoter_v1alpha1.CommitPhaseFailure {
				degraded++
			} else {
				pending++
			}
		}

		//Resolve state
		if healthy == len(stuff) {
			resolvedState = promoter_v1alpha1.CommitPhaseSuccess
			desc = fmt.Sprintf("%d/%d apps are healthy", healthy, len(stuff))
		} else if degraded == len(stuff) {
			resolvedState = promoter_v1alpha1.CommitPhaseFailure
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

func updateAggregatedStatus(kubeClient client.Client, revision string, repo string, sha string, state promoter_v1alpha1.CommitStatusPhase, desc string) error {

	repoURL, err := url.Parse(repo)
	if err != nil {
		panic(err)
	}
	owner := repoURL.Path[1 : strings.Index(repoURL.Path[1:], "/")+1]
	repository := strings.TrimRight(repoURL.Path[strings.LastIndex(repoURL.Path, "/")+1:], ".git")

	commitStatusName := revision + "/health"
	resourceName := strings.ReplaceAll(commitStatusName, "/", "-") + "-" + hash([]byte(repo))

	desiredCommitStatus := promoter_v1alpha1.CommitStatus{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName,
			Namespace: "argocd",
			Labels: map[string]string{
				CommitStatusAnnotation: "healthy",
			},
		},
		Spec: promoter_v1alpha1.CommitStatusSpec{
			RepositoryReference: &promoter_v1alpha1.Repository{
				Owner: owner,
				Name:  repository,
				ScmProviderRef: promoter_v1alpha1.NamespacedObjectReference{
					Name:      "scmprovider-sample",
					Namespace: "default",
				},
			},
			Sha:         sha,
			Name:        commitStatusName,
			Description: desc,
			Phase:       state,
			Url:         "https://example.com",
		},
	}

	currentCommitStatus := promoter_v1alpha1.CommitStatus{}
	err = kubeClient.Get(context.Background(), client.ObjectKey{Namespace: "argocd", Name: resourceName}, &currentCommitStatus)
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
