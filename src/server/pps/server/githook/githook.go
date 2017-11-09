package githook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/pps"
	col "github.com/pachyderm/pachyderm/src/server/pkg/collection"
	"github.com/pachyderm/pachyderm/src/server/pkg/ppsdb"
	"github.com/pachyderm/pachyderm/src/server/pkg/util"

	etcd "github.com/coreos/etcd/clientv3"
	logrus "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"gopkg.in/go-playground/webhooks.v3"
	"gopkg.in/go-playground/webhooks.v3/github"
)

// GitHookPort specifies the port the server will listen on
const GitHookPort = 999
const apiVersion = "v1"

// gitHookServer serves GetFile requests over HTTP
type gitHookServer struct {
	hook       *github.Webhook
	client     *client.APIClient
	etcdClient *etcd.Client
	pipelines  col.Collection
}

// RunGitHookServer starts the webhook server
func RunGitHookServer(address string, etcdAddress string, etcdPrefix string) error {
	c, err := client.NewFromAddress(address)
	if err != nil {
		return err
	}
	etcdClient, err := etcd.New(etcd.Config{
		Endpoints:   []string{etcdAddress},
		DialOptions: client.EtcdDialOptions(),
	})
	if err != nil {
		return err
	}
	hook := github.New(&github.Config{})
	s := &gitHookServer{
		hook,
		c,
		etcdClient,
		ppsdb.Pipelines(etcdClient, etcdPrefix),
	}

	hook.RegisterEvents(
		func(payload interface{}, header webhooks.Header) {
			if err := s.HandlePush(payload, header); err != nil {
				pl := payload.(github.PushPayload)
				logrus.Infof("github webhook failed to handle push for repo (%v) on branch (%v) with error %v", pl.Repository.Name, path.Base(pl.Ref), err)
			}
		},
		github.PushEvent,
	)
	return webhooks.Run(hook, ":"+strconv.Itoa(GitHookPort), fmt.Sprintf("/%v/handle/push", apiVersion))
}
func matchingBranch(inputBranch string, payloadBranch string) bool {
	if inputBranch == payloadBranch {
		return true
	}
	if inputBranch == "" && payloadBranch == "master" {
		return true
	}
	return false
}

func (s *gitHookServer) findMatchingPipelineInputs(payload github.PushPayload) (pipelines []*pps.PipelineInfo, inputs []*pps.GitInput, err error) {
	payloadBranch := path.Base(payload.Ref)
	pipelines, err = s.client.ListPipeline()
	if err != nil {
		return nil, nil, err
	}
	for _, pipelineInfo := range pipelines {
		pps.VisitInput(pipelineInfo.Input, func(input *pps.Input) {
			if input.Git != nil {
				if input.Git.URL == payload.Repository.CloneURL && matchingBranch(input.Git.Branch, payloadBranch) {
					inputs = append(inputs, input.Git)
				}
			}
		})
	}
	if len(inputs) == 0 {
		return nil, nil, fmt.Errorf("no pipeline inputs corresponding to git URL (%v) on branch (%v) found, perhaps the git input is not set yet on a pipeline", payload.Repository.CloneURL, payloadBranch)
	}
	return pipelines, inputs, nil
}

func (s *gitHookServer) HandlePush(payload interface{}, _ webhooks.Header) (retErr error) {
	pl := payload.(github.PushPayload)
	logrus.Infof("received github push payload for repo (%v) on branch (%v)", pl.Repository.Name, path.Base(pl.Ref))

	raw, err := json.Marshal(pl)
	if err != nil {
		return fmt.Errorf("error marshalling payload (%v): %v", pl, err)
	}
	pipelines, gitInputs, err := s.findMatchingPipelineInputs(pl)
	if err != nil {
		return err
	}
	if pl.Repository.Private {
		for _, pipelineInfo := range pipelines {
			err := util.FailPipeline(context.Background(), s.etcdClient, s.pipelines, pipelineInfo.Pipeline.Name, fmt.Sprintf("unable to clone private github repo (%v)", pl.Repository.CloneURL))
			// err will be handled but first we want to
			// try and fail all relevant pipelines
			if err != nil {
				logrus.Errorf("error marking pipeline %v as failed %v", pipelineInfo.Pipeline.Name, err)
				retErr = err
			}
		}
		return retErr
	}
	triggeredRepos := make(map[string]bool)
	for _, input := range gitInputs {
		repoName := pps.RepoNameFromGitInfo(input.URL, input.Name)
		if triggeredRepos[repoName] {
			// This input is used on multiple pipelines, and we've already
			// committed to this input repo
			continue
		}
		err := s.commitPayload(repoName, input.Branch, raw)
		if err != nil {
			logrus.Errorf("github webhook failed to commit payload to repo (%v) push with error: %v\n", repoName, err)
			retErr = err
			continue
		}
		triggeredRepos[repoName] = true
	}
	return nil
}

func (s *gitHookServer) commitPayload(repoName string, branchName string, rawPayload []byte) (retErr error) {
	commit, err := s.client.StartCommit(repoName, branchName)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			err := s.client.DeleteCommit(repoName, commit.ID)
			if err != nil {
				logrus.Errorf("git webhook failed to delete partial commit (%v) on repo (%v)", commit.ID, repoName)
			}
			return
		}
		retErr = s.client.FinishCommit(repoName, commit.ID)
	}()
	if err = s.client.DeleteFile(repoName, commit.ID, "commit.json"); err != nil {
		return err
	}
	if _, err = s.client.PutFile(repoName, commit.ID, "commit.json", bytes.NewReader(rawPayload)); err != nil {
		return err
	}
	return nil
}
