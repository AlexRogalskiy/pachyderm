package pps

import (
	"fmt"

	"github.com/pachyderm/pachyderm/src/pfs"
	ppsclient "github.com/pachyderm/pachyderm/src/client/pps"
)

func JobRepo(job *ppsclient.Job) *pfs.Repo {
	return &pfs.Repo{Name: fmt.Sprintf("job-%s", job.ID)}
}

func PipelineRepo(pipeline *ppsclient.Pipeline) *pfs.Repo {
	return &pfs.Repo{Name: pipeline.Name}
}
