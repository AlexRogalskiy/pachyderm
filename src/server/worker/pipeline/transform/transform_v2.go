package transform

import (
	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/server/worker/driver"
	"github.com/pachyderm/pachyderm/src/server/worker/logs"
)

// RunV2 will run a transform pipeline until the driver is canceled.
func RunV2(driver driver.Driver, logger logs.TaggedLogger) error {
	reg, err := newRegistryV2(driver, logger)
	if err != nil {
		return err
	}
	logger.Logf("transform spawner started")
	return forEachCommitV2(driver, func(commitInfo *pfs.CommitInfo, metaCommit *pfs.Commit) error {
		return reg.startJob(commitInfo, metaCommit)
	})
}

func getStatsCommitV2(commitInfo *pfs.CommitInfo) *pfs.Commit {
	for _, commitRange := range commitInfo.Subvenance {
		if commitRange.Lower.Repo.Name == commitInfo.Commit.Repo.Name {
			return commitRange.Lower
		}
	}
	return nil
}

func forEachCommitV2(driver driver.Driver, cb func(*pfs.CommitInfo, *pfs.Commit) error) error {
	pachClient := driver.PachClient()
	pi := driver.PipelineInfo()
	// TODO: Readd subscribe on spec commit provenance. Current code simplifies correctness in terms
	// of commits being closed / jobs being finished.
	return pachClient.SubscribeCommitF(
		pi.Pipeline.Name,
		"",
		nil,
		"",
		pfs.CommitState_READY,
		func(ci *pfs.CommitInfo) error {
			return cb(ci, getStatsCommitV2(ci))
		},
	)
}
