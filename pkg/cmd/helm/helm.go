package helm

import (
	"github.com/jenkins-x/jx-gitops/pkg/cmd/helm/build"
	"github.com/jenkins-x/jx-gitops/pkg/cmd/helm/release"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras"
	"github.com/jenkins-x/jx-logging/v3/pkg/log"
	"github.com/spf13/cobra"
)

// NewCmdHelm creates the new command
func NewCmdHelm() *cobra.Command {
	command := &cobra.Command{
		Use:   "helm",
		Short: "Commands for working with helm charts",
		Run: func(command *cobra.Command, args []string) {
			err := command.Help()
			if err != nil {
				log.Logger().Errorf(err.Error())
			}
		},
	}
	command.AddCommand(cobras.SplitCommand(NewCmdHelmTemplate()))
	command.AddCommand(cobras.SplitCommand(build.NewCmdHelmBuild()))
	command.AddCommand(cobras.SplitCommand(release.NewCmdHelmRelease()))
	return command
}
