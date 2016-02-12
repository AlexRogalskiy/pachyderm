package pachyderm

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/context"

	"github.com/pachyderm/pachyderm/src/pfs"
	"github.com/pachyderm/pachyderm/src/pfs/pfsutil"
	"github.com/pachyderm/pachyderm/src/pkg/require"
	"github.com/pachyderm/pachyderm/src/pkg/uuid"
	"github.com/pachyderm/pachyderm/src/pkg/workload"
	"github.com/pachyderm/pachyderm/src/pps"
	"github.com/pachyderm/pachyderm/src/pps/ppsutil"
	"google.golang.org/grpc"
)

const (
	NUMFILES = 25
	KB       = 1024 * 1024
)

func TestJob(t *testing.T) {
	t.Parallel()
	dataRepo := uniqueString("TestJob.data")
	pachClient := getPachClient(t)
	require.NoError(t, pfsutil.CreateRepo(pachClient, dataRepo))
	commit, err := pfsutil.StartCommit(pachClient, dataRepo, "")
	require.NoError(t, err)
	_, err = pfsutil.PutFile(pachClient, dataRepo, commit.Id, "file", 0, strings.NewReader("foo\n"))
	require.NoError(t, err)
	require.NoError(t, pfsutil.FinishCommit(pachClient, dataRepo, commit.Id))
	job, err := ppsutil.CreateJob(
		pachClient,
		"",
		[]string{"cp", path.Join("/pfs", dataRepo, "file"), "/pfs/out/file"},
		"",
		1,
		[]*pps.JobInput{{Commit: commit}},
		"",
	)
	require.NoError(t, err)
	inspectJobRequest := &pps.InspectJobRequest{
		Job:         job,
		BlockOutput: true,
		BlockState:  true,
	}
	jobInfo, err := pachClient.InspectJob(context.Background(), inspectJobRequest)
	require.NoError(t, err)
	require.Equal(t, pps.JobState_JOB_STATE_SUCCESS.String(), jobInfo.State.String())
	commitInfo, err := pfsutil.InspectCommit(pachClient, jobInfo.OutputCommit.Repo.Name, jobInfo.OutputCommit.Id)
	require.NoError(t, err)
	require.Equal(t, pfs.CommitType_COMMIT_TYPE_READ, commitInfo.CommitType)
	var buffer bytes.Buffer
	require.NoError(t, pfsutil.GetFile(pachClient, jobInfo.OutputCommit.Repo.Name, jobInfo.OutputCommit.Id, "file", 0, 0, "", nil, &buffer))
	require.Equal(t, "foo\n", buffer.String())
}

func TestGrep(t *testing.T) {
	t.Parallel()
	dataRepo := uniqueString("TestGrep.data")
	pachClient := getPachClient(t)
	require.NoError(t, pfsutil.CreateRepo(pachClient, dataRepo))
	commit, err := pfsutil.StartCommit(pachClient, dataRepo, "")
	require.NoError(t, err)
	for i := 0; i < 100; i++ {
		_, err = pfsutil.PutFile(pachClient, dataRepo, commit.Id, fmt.Sprintf("file%d", i), 0, strings.NewReader("foo\nbar\nfizz\nbuzz\n"))
		require.NoError(t, err)
	}
	require.NoError(t, pfsutil.FinishCommit(pachClient, dataRepo, commit.Id))
	job1, err := ppsutil.CreateJob(
		pachClient,
		"",
		[]string{"bash"},
		fmt.Sprintf("grep foo /pfs/%s/* >/pfs/out/foo", dataRepo),
		1,
		[]*pps.JobInput{{Commit: commit}},
		"",
	)
	require.NoError(t, err)
	job2, err := ppsutil.CreateJob(
		pachClient,
		"",
		[]string{"bash"},
		fmt.Sprintf("grep foo /pfs/%s/* >/pfs/out/foo", dataRepo),
		4,
		[]*pps.JobInput{{Commit: commit}},
		"",
	)
	require.NoError(t, err)
	inspectJobRequest := &pps.InspectJobRequest{
		Job:         job1,
		BlockOutput: true,
		BlockState:  true,
	}
	job1Info, err := pachClient.InspectJob(context.Background(), inspectJobRequest)
	require.NoError(t, err)
	inspectJobRequest.Job = job2
	job2Info, err := pachClient.InspectJob(context.Background(), inspectJobRequest)
	require.NoError(t, err)
	repo1Info, err := pfsutil.InspectRepo(pachClient, job1Info.OutputCommit.Repo.Name)
	require.NoError(t, err)
	repo2Info, err := pfsutil.InspectRepo(pachClient, job2Info.OutputCommit.Repo.Name)
	require.NoError(t, err)
	require.Equal(t, repo1Info.SizeBytes, repo2Info.SizeBytes)
}

func TestPipeline(t *testing.T) {
	t.Parallel()
	pachClient := getPachClient(t)
	// create repos
	dataRepo := uniqueString("TestPipeline.data")
	require.NoError(t, pfsutil.CreateRepo(pachClient, dataRepo))
	// create pipeline
	pipelineName := uniqueString("pipeline")
	outRepo := pps.PipelineRepo(ppsutil.NewPipeline(pipelineName))
	require.NoError(t, ppsutil.CreatePipeline(
		pachClient,
		pipelineName,
		"",
		[]string{"cp", path.Join("/pfs", dataRepo, "file"), "/pfs/out/file"},
		"",
		1,
		[]*pps.PipelineInput{{Repo: &pfs.Repo{Name: dataRepo}}},
	))
	// Do first commit to repo
	commit1, err := pfsutil.StartCommit(pachClient, dataRepo, "")
	require.NoError(t, err)
	_, err = pfsutil.PutFile(pachClient, dataRepo, commit1.Id, "file", 0, strings.NewReader("foo\n"))
	require.NoError(t, err)
	require.NoError(t, pfsutil.FinishCommit(pachClient, dataRepo, commit1.Id))
	listCommitRequest := &pfs.ListCommitRequest{
		Repo:       []*pfs.Repo{outRepo},
		CommitType: pfs.CommitType_COMMIT_TYPE_READ,
		Block:      true,
	}
	listCommitResponse, err := pachClient.ListCommit(
		context.Background(),
		listCommitRequest,
	)
	require.NoError(t, err)
	outCommits := listCommitResponse.CommitInfo
	require.Equal(t, 1, len(outCommits))
	var buffer bytes.Buffer
	require.NoError(t, pfsutil.GetFile(pachClient, outRepo.Name, outCommits[0].Commit.Id, "file", 0, 0, "", nil, &buffer))
	require.Equal(t, "foo\n", buffer.String())
	// Do second commit to repo
	commit2, err := pfsutil.StartCommit(pachClient, dataRepo, commit1.Id)
	require.NoError(t, err)
	_, err = pfsutil.PutFile(pachClient, dataRepo, commit2.Id, "file", 0, strings.NewReader("bar\n"))
	require.NoError(t, err)
	require.NoError(t, pfsutil.FinishCommit(pachClient, dataRepo, commit2.Id))
	listCommitRequest = &pfs.ListCommitRequest{
		Repo:       []*pfs.Repo{outRepo},
		FromCommit: []*pfs.Commit{outCommits[0].Commit},
		CommitType: pfs.CommitType_COMMIT_TYPE_READ,
		Block:      true,
	}
	listCommitResponse, err = pachClient.ListCommit(
		context.Background(),
		listCommitRequest,
	)
	require.NoError(t, err)
	require.NotNil(t, listCommitResponse.CommitInfo[0].ParentCommit)
	require.Equal(t, outCommits[0].Commit.Id, listCommitResponse.CommitInfo[0].ParentCommit.Id)
	outCommits = listCommitResponse.CommitInfo
	require.Equal(t, 1, len(outCommits))
	buffer = bytes.Buffer{}
	require.NoError(t, pfsutil.GetFile(pachClient, outRepo.Name, outCommits[0].Commit.Id, "file", 0, 0, "", nil, &buffer))
	require.Equal(t, "foo\nbar\n", buffer.String())
}

func TestWorkload(t *testing.T) {
	t.Parallel()
	pachClient := getPachClient(t)
	seed := time.Now().UnixNano()
	require.NoError(t, workload.RunWorkload(pachClient, pachClient, rand.New(rand.NewSource(seed)), 100))
}

func TestSharding(t *testing.T) {
	t.Parallel()
	repo := uniqueString("TestSharding")
	pachClient := getPachClient(t)
	err := pfsutil.CreateRepo(pachClient, repo)
	require.NoError(t, err)
	commit, err := pfsutil.StartCommit(pachClient, repo, "")
	require.NoError(t, err)
	var wg sync.WaitGroup
	for i := 0; i < NUMFILES; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			rand := rand.New(rand.NewSource(int64(i)))
			_, err = pfsutil.PutFile(pachClient, repo, commit.Id, fmt.Sprintf("file%d", i), 0, workload.NewReader(rand, KB))
			require.NoError(t, err)
		}()
	}
	wg.Wait()
	err = pfsutil.FinishCommit(pachClient, repo, commit.Id)
	require.NoError(t, err)
	wg = sync.WaitGroup{}
	for i := 0; i < NUMFILES; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buffer1Shard bytes.Buffer
			var buffer4Shard bytes.Buffer
			shard := &pfs.Shard{FileModulus: 1, BlockModulus: 1}
			err := pfsutil.GetFile(pachClient, repo, commit.Id,
				fmt.Sprintf("file%d", i), 0, 0, "", shard, &buffer1Shard)
			require.NoError(t, err)
			shard.BlockModulus = 4
			for blockNumber := uint64(0); blockNumber < 4; blockNumber++ {
				shard.BlockNumber = blockNumber
				err := pfsutil.GetFile(pachClient, repo, commit.Id,
					fmt.Sprintf("file%d", i), 0, 0, "", shard, &buffer4Shard)
				require.NoError(t, err)
			}
			require.Equal(t, buffer1Shard.Len(), buffer4Shard.Len())
		}()
	}
	wg.Wait()
}

func TestFromCommit(t *testing.T) {
	t.Parallel()
	repo := uniqueString("TestFromCommit")
	pachClient := getPachClient(t)
	seed := time.Now().UnixNano()
	rand := rand.New(rand.NewSource(seed))
	err := pfsutil.CreateRepo(pachClient, repo)
	require.NoError(t, err)
	commit1, err := pfsutil.StartCommit(pachClient, repo, "")
	require.NoError(t, err)
	_, err = pfsutil.PutFile(pachClient, repo, commit1.Id, "file", 0, workload.NewReader(rand, KB))
	require.NoError(t, err)
	err = pfsutil.FinishCommit(pachClient, repo, commit1.Id)
	require.NoError(t, err)
	commit2, err := pfsutil.StartCommit(pachClient, repo, commit1.Id)
	require.NoError(t, err)
	_, err = pfsutil.PutFile(pachClient, repo, commit2.Id, "file", 0, workload.NewReader(rand, KB))
	require.NoError(t, err)
	err = pfsutil.FinishCommit(pachClient, repo, commit2.Id)
	require.NoError(t, err)
	var buffer bytes.Buffer
	require.NoError(t, pfsutil.GetFile(pachClient, repo, commit2.Id, "file", 0, 0, commit1.Id, nil, &buffer))
	require.Equal(t, buffer.Len(), KB)
	buffer = bytes.Buffer{}
	require.NoError(t, pfsutil.GetFile(pachClient, repo, commit2.Id, "file", 0, 0, "", nil, &buffer))
	require.Equal(t, buffer.Len(), 2*KB)
}

func getPachClient(t *testing.T) *APIClient {
	pachAddr := os.Getenv("PACHD_PORT_650_TCP_ADDR")
	if pachAddr == "" {
		t.Error("PACHD_PORT_650_TCP_ADDR not set")
	}
	clientConn, err := grpc.Dial(fmt.Sprintf("%s:650", pachAddr), grpc.WithInsecure())
	require.NoError(t, err)
	return NewAPIClient(clientConn)
}

func uniqueString(prefix string) string {
	return prefix + "." + uuid.NewWithoutDashes()[0:12]
}
