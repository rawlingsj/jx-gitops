package activities

import (
	"context"
	"sort"
	"strings"
	"time"

	v1 "github.com/jenkins-x/jx-api/v4/pkg/apis/jenkins.io/v1"
	jxc "github.com/jenkins-x/jx-api/v4/pkg/client/clientset/versioned"
	jv1 "github.com/jenkins-x/jx-api/v4/pkg/client/clientset/versioned/typed/jenkins.io/v1"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/helper"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/templates"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube/jxclient"
	"github.com/jenkins-x/jx-helpers/v3/pkg/termcolor"
	"github.com/jenkins-x/jx-logging/v3/pkg/log"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Options command line arguments and flags
type Options struct {
	DryRun                  bool
	ReleaseHistoryLimit     int
	PullRequestHistoryLimit int
	ReleaseAgeLimit         time.Duration
	PullRequestAgeLimit     time.Duration
	PipelineRunAgeLimit     time.Duration
	ProwJobAgeLimit         time.Duration
	Namespace               string
	JXClient                jxc.Interface
}

var (
	info = termcolor.ColorInfo

	cmdLong = templates.LongDesc(`
		Garbage collect the Jenkins X PipelineActivity resources

`)

	cmdExample = templates.Examples(`
		# garbage collect PipelineActivity resources
		jx gitops gc activities

		# dry run mode
		jx gitops gc pa --dry-run
`)
)

type buildCounter struct {
	ReleaseCount int
	PRCount      int
}

type buildsCount struct {
	cache map[string]*buildCounter
}

// AddBuild adds the build and returns the number of builds for this repo and branch
func (c *buildsCount) AddBuild(repoAndBranch string, isPR bool) int {
	if c.cache == nil {
		c.cache = map[string]*buildCounter{}
	}
	bc := c.cache[repoAndBranch]
	if bc == nil {
		bc = &buildCounter{}
		c.cache[repoAndBranch] = bc
	}
	if isPR {
		bc.PRCount++
		return bc.PRCount
	}
	bc.ReleaseCount++
	return bc.ReleaseCount
}

// NewCmd s a command object for the "step" command
func NewCmdGCActivities() (*cobra.Command, *Options) {
	o := &Options{}

	cmd := &cobra.Command{
		Use:     "activities",
		Aliases: []string{"pa", "act", "pr"},
		Short:   "garbage collection for PipelineActivity resources",
		Long:    cmdLong,
		Example: cmdExample,
		Run: func(cmd *cobra.Command, args []string) {
			err := o.Run()
			helper.CheckErr(err)
		},
	}
	cmd.Flags().BoolVarP(&o.DryRun, "dry-run", "d", false, "Dry run mode. If enabled just list the resources that would be removed")
	cmd.Flags().IntVarP(&o.ReleaseHistoryLimit, "release-history-limit", "l", 5, "Maximum number of PipelineActivities to keep around per repository release")
	cmd.Flags().IntVarP(&o.PullRequestHistoryLimit, "pr-history-limit", "", 2, "Minimum number of PipelineActivities to keep around per repository Pull Request")
	cmd.Flags().DurationVarP(&o.PullRequestAgeLimit, "pull-request-age", "p", time.Hour*48, "Maximum age to keep PipelineActivities for Pull Requests")
	cmd.Flags().DurationVarP(&o.ReleaseAgeLimit, "release-age", "r", time.Hour*24*30, "Maximum age to keep PipelineActivities for Releases")
	cmd.Flags().DurationVarP(&o.PipelineRunAgeLimit, "pipelinerun-age", "", time.Hour*12, "Maximum age to keep completed PipelineRuns for all pipelines")
	cmd.Flags().DurationVarP(&o.ProwJobAgeLimit, "prowjob-age", "", time.Hour*24*7, "Maximum age to keep completed ProwJobs for all pipelines")
	return cmd, o
}

// Run implements this command
func (o *Options) Run() error {
	var err error
	o.JXClient, o.Namespace, err = jxclient.LazyCreateJXClientAndNamespace(o.JXClient, o.Namespace)
	if err != nil {
		return errors.Wrapf(err, "failed to create jx client")
	}

	client := o.JXClient
	currentNs := o.Namespace
	ctx := context.TODO()

	// cannot use field selectors like `spec.kind=Preview` on CRDs so list all environments
	activityInterface := client.JenkinsV1().PipelineActivities(currentNs)
	activities, err := activityInterface.List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	if len(activities.Items) == 0 {
		// no preview environments found so lets return gracefully
		log.Logger().Debug("no activities found")
		return nil
	}

	now := time.Now()
	counters := &buildsCount{}

	var completedActivities []v1.PipelineActivity

	// Filter out running activities
	for _, a := range activities.Items {
		if a.Spec.CompletedTimestamp != nil {
			completedActivities = append(completedActivities, a)
		}
	}

	// Sort with newest created activities first
	sort.Slice(completedActivities, func(i, j int) bool {
		return !completedActivities[i].Spec.CompletedTimestamp.Before(completedActivities[j].Spec.CompletedTimestamp)
	})

	//
	for _, a := range completedActivities {
		activity := a
		branchName := a.BranchName()
		isPR, isBatch := o.isPullRequestOrBatchBranch(branchName)
		maxAge, revisionHistory := o.ageAndHistoryLimits(isPR, isBatch)
		// lets remove activities that are too old
		if activity.Spec.CompletedTimestamp != nil && activity.Spec.CompletedTimestamp.Add(maxAge).Before(now) {

			err = o.deleteActivity(ctx, activityInterface, &activity)
			if err != nil {
				return err
			}
			continue
		}

		repoBranchAndContext := activity.RepositoryOwner() + "/" + activity.RepositoryName() + "/" + activity.BranchName() + "/" + activity.Spec.Context
		c := counters.AddBuild(repoBranchAndContext, isPR)
		if c > revisionHistory && a.Spec.CompletedTimestamp != nil {
			err = o.deleteActivity(ctx, activityInterface, &activity)
			if err != nil {
				return err
			}
			continue
		}
	}

	// Clean up completed PipelineRuns
	/*
		err = o.gcPipelineRuns(currentNs)
		if err != nil {
			return err
		}
	*/

	return nil
}

func (o *Options) deleteActivity(ctx context.Context, activityInterface jv1.PipelineActivityInterface, a *v1.PipelineActivity) error {
	prefix := ""
	if o.DryRun {
		prefix = "not "
	}
	log.Logger().Infof("%sdeleting PipelineActivity %s", prefix, info(a.Name))
	if o.DryRun {
		return nil
	}
	return activityInterface.Delete(ctx, a.Name, *metav1.NewDeleteOptions(0))
}

func (o *Options) ageAndHistoryLimits(isPR, isBatch bool) (time.Duration, int) {
	maxAge := o.ReleaseAgeLimit
	revisionLimit := o.ReleaseHistoryLimit
	if isPR || isBatch {
		maxAge = o.PullRequestAgeLimit
		revisionLimit = o.PullRequestHistoryLimit
	}
	return maxAge, revisionLimit
}

func (o *Options) isPullRequestOrBatchBranch(branchName string) (bool, bool) {
	return strings.HasPrefix(branchName, "PR-"), branchName == "batch"
}
