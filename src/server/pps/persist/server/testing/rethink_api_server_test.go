package testing

import (
	"testing"

	pfsserver "github.com/pachyderm/pachyderm/src/server/pfs"
	pfsclient "github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/pkg/require"
	"github.com/pachyderm/pachyderm/src/pkg/uuid"
	ppsclient "github.com/pachyderm/pachyderm/src/client/pps"
	"github.com/pachyderm/pachyderm/src/pps/persist"
	"golang.org/x/net/context"
)

func TestBasicRethink(t *testing.T) {
	RunTestWithRethinkAPIServer(t, testBasicRethink)
}

func TestBlock(t *testing.T) {
	RunTestWithRethinkAPIServer(t, testBlock)
}

func testBasicRethink(t *testing.T, apiServer persist.APIServer) {
	_, err := apiServer.CreatePipelineInfo(
		context.Background(),
		&persist.PipelineInfo{
			PipelineName: "foo",
		},
	)
	require.NoError(t, err)
	pipelineInfo, err := apiServer.GetPipelineInfo(
		context.Background(),
		&ppsclient.Pipeline{Name: "foo"},
	)
	require.NoError(t, err)
	require.Equal(t, pipelineInfo.PipelineName, "foo")
	input := &ppsclient.JobInput{Commit: pfsclient.NewCommit("bar", uuid.NewWithoutDashes())}
	jobInfo, err := apiServer.CreateJobInfo(
		context.Background(),
		&persist.JobInfo{
			PipelineName: "foo",
			Inputs:       []*ppsclient.JobInput{input},
		},
	)
	jobID := jobInfo.JobID
	input2 := &ppsclient.JobInput{Commit: pfsclient.NewCommit("fizz", uuid.NewWithoutDashes())}

	_, err = apiServer.CreateJobInfo(
		context.Background(),
		&persist.JobInfo{
			PipelineName: "buzz",
			Inputs:       []*ppsclient.JobInput{input2},
		},
	)
	require.NoError(t, err)
	jobInfo, err = apiServer.InspectJob(
		context.Background(),
		&ppsclient.InspectJobRequest{
			Job: &ppsclient.Job{
				ID: jobInfo.JobID,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, jobInfo.JobID, jobID)
	require.Equal(t, "foo", jobInfo.PipelineName)
	jobInfos, err := apiServer.ListJobInfos(
		context.Background(),
		&ppsclient.ListJobRequest{
			Pipeline: &ppsclient.Pipeline{Name: "foo"},
		},
	)
	require.NoError(t, err)
	require.Equal(t, len(jobInfos.JobInfo), 1)
	require.Equal(t, jobInfos.JobInfo[0].JobID, jobID)
	jobInfos, err = apiServer.ListJobInfos(
		context.Background(),
		&ppsclient.ListJobRequest{
			InputCommit: []*pfsserver.Commit{input.Commit},
		},
	)
	require.NoError(t, err)
	require.Equal(t, len(jobInfos.JobInfo), 1)
	require.Equal(t, jobInfos.JobInfo[0].JobID, jobID)
	jobInfos, err = apiServer.ListJobInfos(
		context.Background(),
		&ppsclient.ListJobRequest{
			Pipeline:    &ppsclient.Pipeline{Name: "foo"},
			InputCommit: []*pfsserver.Commit{input.Commit},
		},
	)
	require.NoError(t, err)
	require.Equal(t, len(jobInfos.JobInfo), 1)
	require.Equal(t, jobInfos.JobInfo[0].JobID, jobID)
}

func testBlock(t *testing.T, apiServer persist.APIServer) {
	jobInfo, err := apiServer.CreateJobInfo(context.Background(), &persist.JobInfo{})
	require.NoError(t, err)
	jobID := jobInfo.JobID
	go func() {
		_, err := apiServer.CreateJobOutput(
			context.Background(),
			&persist.JobOutput{
				JobID:        jobID,
				OutputCommit: pfsclient.NewCommit("foo", "bar"),
			})
		require.NoError(t, err)
		_, err = apiServer.CreateJobState(
			context.Background(),
			&persist.JobState{
				JobID: jobID,
				State: ppsclient.JobState_JOB_STATE_SUCCESS,
			})
		require.NoError(t, err)
	}()
	_, err = apiServer.InspectJob(
		context.Background(),
		&ppsclient.InspectJobRequest{
			Job:         &ppsclient.Job{ID: jobID},
			BlockOutput: true,
			BlockState:  true,
		},
	)
	require.NoError(t, err)
}
