// +build linux

package cmd

import (
	"github.com/spf13/cobra"

	. "github.com/openshift/geard/cmd"
	"github.com/openshift/geard/containers"
	"github.com/openshift/geard/git"
	gitjobs "github.com/openshift/geard/git/jobs"
)

func RegisterInitRepo(parent *cobra.Command) {
	initRepoCmd := &cobra.Command{
		Use:   "init-repo <name> [<url>]",
		Short: `(Local) Setup the environment for a git repository`,
		Long:  ``,
		Run:   initRepository,
	}
	parent.AddCommand(initRepoCmd)
}

func initRepository(cmd *cobra.Command, args []string) {
	if len(args) < 1 || len(args) > 2 {
		Fail(1, "Valid arguments: <repo_id> [<repo_url>]\n")
	}

	repoId, err := containers.NewIdentifier(args[0])
	if err != nil {
		Fail(1, "Argument 1 must be a valid repository identifier: %s\n", err.Error())
	}

	repoUrl := ""
	if len(args) == 2 {
		repoUrl = args[1]
	}

	if err := gitjobs.InitializeRepository(git.RepoIdentifier(repoId), repoUrl); err != nil {
		Fail(2, "Unable to initialize repository %s\n", err.Error())
	}
}
