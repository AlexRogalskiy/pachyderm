package server

import (
	"fmt"
	"sort"
	"time"

	"github.com/dancannon/gorethink"
	"github.com/golang/protobuf/proto"
	"github.com/pachyderm/pachyderm/src/pfs"
	"github.com/pachyderm/pachyderm/src/pkg/uuid"
	"github.com/pachyderm/pachyderm/src/pps"
	"github.com/pachyderm/pachyderm/src/pps/persist"
	"go.pedge.io/google-protobuf"
	"go.pedge.io/pkg/time"
	"go.pedge.io/proto/rpclog"
	"go.pedge.io/proto/time"
	"golang.org/x/net/context"
)

const (
	jobInfosTable      Table = "JobInfos"
	pipelineInfosTable Table = "PipelineInfos"

	pipelineNameIndex          Index = "PipelineName"
	pipelineNameAndCommitIndex Index = "PipelineNameAndCommitIndex"
	commitIndex                Index = "CommitIndex"
)

type Table string
type PrimaryKey string
type Index string

var (
	tables = []Table{
		jobInfosTable,
		pipelineInfosTable,
	}

	tableToTableCreateOpts = map[Table][]gorethink.TableCreateOpts{
		jobInfosTable: []gorethink.TableCreateOpts{
			gorethink.TableCreateOpts{
				PrimaryKey: "JobId",
			},
		},
		pipelineInfosTable: []gorethink.TableCreateOpts{
			gorethink.TableCreateOpts{
				PrimaryKey: "PipelineName",
			},
		},
	}
)

// InitDBs prepares a RethinkDB instance to be used by the rethink server.
// Rethink servers will error if they are pointed at databases that haven't had InitDBs run on them.
// InitDBs is idempotent (unless rethink dies in the middle of the function)
func InitDBs(address string, databaseName string) error {
	session, err := gorethink.Connect(gorethink.ConnectOpts{Address: address})
	if err != nil {
		return err
	}
	if _, err := gorethink.DBCreate(databaseName).RunWrite(session); err != nil {
		if _, ok := err.(gorethink.RQLRuntimeError); ok {
			return nil
		}
		return err
	}
	for _, table := range tables {
		tableCreateOpts, ok := tableToTableCreateOpts[table]
		if ok {
			if _, err := gorethink.DB(databaseName).TableCreate(table, tableCreateOpts...).RunWrite(session); err != nil {
				return err
			}
		} else {
			if _, err := gorethink.DB(databaseName).TableCreate(table).RunWrite(session); err != nil {
				return err
			}
		}
	}

	// Create some indexes for the jobInfosTable
	if _, err := gorethink.DB(databaseName).Table(jobInfosTable).IndexCreate(pipelineNameIndex).RunWrite(session); err != nil {
		return err
	}
	if _, err := gorethink.DB(databaseName).Table(jobInfosTable).IndexCreate(commitIndex).RunWrite(session); err != nil {
		return err
	}
	if _, err := gorethink.DB(databaseName).Table(jobInfosTable).IndexCreateFunc(
		pipelineNameAndCommitIndex,
		func(row gorethink.Term) interface{} {
			return []interface{}{
				row.Field(pipelineNameIndex),
				row.Field(commitIndex),
			}
		}).RunWrite(session); err != nil {
		return err
	}
	return nil
}

type rethinkAPIServer struct {
	protorpclog.Logger
	session      *gorethink.Session
	databaseName string
	timer        pkgtime.Timer
}

func newRethinkAPIServer(address string, databaseName string) (*rethinkAPIServer, error) {
	session, err := gorethink.Connect(gorethink.ConnectOpts{Address: address})
	if err != nil {
		return nil, err
	}
	return &rethinkAPIServer{
		protorpclog.NewLogger("pachyderm.pps.persist.API"),
		session,
		databaseName,
		pkgtime.NewSystemTimer(),
	}, nil
}

func (a *rethinkAPIServer) Close() error {
	return a.session.Close()
}

// JobId cannot be set
// Timestamp cannot be set
func (a *rethinkAPIServer) CreateJobInfo(ctx context.Context, request *persist.JobInfo) (response *persist.JobInfo, err error) {
	defer func(start time.Time) { a.Log(request, response, err, time.Since(start)) }(time.Now())
	if request.JobId != "" {
		return nil, fmt.Errorf("request.JobId should be unset")
	}
	if request.CreatedAt != nil {
		return nil, fmt.Errorf("request.CreatedAt should be unset")
	}
	if request.CommitIndex != "" {
		return nil, fmt.Errorf("request.CommitIndex should be unset")
	}
	request.JobId = uuid.NewWithoutDashes()
	request.CreatedAt = prototime.TimeToTimestamp(time.Now())
	request.CommitIndex = genCommitIndex(request.InputCommit)
	if err := a.insertMessage(jobInfosTable, request); err != nil {
		return nil, err
	}
	return request, nil
}

func (a *rethinkAPIServer) InspectJob(ctx context.Context, request *pps.InspectJobRequest) (response *persist.JobInfo, err error) {
	defer func(start time.Time) { a.Log(request, response, err, time.Since(start)) }(time.Now())
	jobInfo := &persist.JobInfo{}
	var mustHaveFields []interface{}
	if request.BlockOutput {
		mustHaveFields = append(mustHaveFields, "OutputCommit")
	}
	if request.BlockState {
		mustHaveFields = append(mustHaveFields, "State")
	}
	if err := a.waitMessageByPrimaryKey(
		jobInfosTable,
		request.Job.Id,
		jobInfo,
		func(jobInfo gorethink.Term) gorethink.Term {
			blockOutput := jobInfo.HasFields("OutputCommit")
			blockState := jobInfo.Field("State").Ne(pps.JobState_JOB_STATE_RUNNING)
			if request.BlockOutput && request.BlockState {
				return blockOutput.And(blockState)
			} else if request.BlockOutput {
				return blockOutput
			} else if request.BlockState {
				return blockState
			}
			return gorethink.Expr(true)
		},
	); err != nil {
		return nil, err
	}
	return jobInfo, nil
}

func (a *rethinkAPIServer) ListJobInfos(ctx context.Context, request *pps.ListJobRequest) (response *persist.JobInfos, retErr error) {
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	query := a.getTerm(jobInfosTable)
	if request.Pipeline != nil && len(request.InputCommit) > 0 {
		query = query.GetAllByIndex(
			pipelineNameAndCommitIndex,
			gorethink.Expr([]interface{}{request.Pipeline.Name, genCommitIndex(request.InputCommit)}),
		)
	} else if request.Pipeline != nil {
		query = query.GetAllByIndex(
			pipelineNameIndex,
			request.Pipeline.Name,
		)
	} else if len(request.InputCommit) > 0 {
		query = query.GetAllByIndex(
			commitIndex,
			gorethink.Expr(genCommitIndex(request.InputCommit)),
		)
	}
	cursor, err := query.Run(a.session)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := cursor.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	result := &persist.JobInfos{}
	for {
		jobInfo := &persist.JobInfo{}
		if !cursor.Next(jobInfo) {
			break
		}
		result.JobInfo = append(result.JobInfo, jobInfo)
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (a *rethinkAPIServer) DeleteJobInfo(ctx context.Context, request *pps.Job) (response *google_protobuf.Empty, err error) {
	defer func(start time.Time) { a.Log(request, response, err, time.Since(start)) }(time.Now())
	if err := a.deleteMessageByPrimaryKey(jobInfosTable, request.Id); err != nil {
		return nil, err
	}
	return google_protobuf.EmptyInstance, nil
}

func (a *rethinkAPIServer) CreateJobOutput(ctx context.Context, request *persist.JobOutput) (response *google_protobuf.Empty, err error) {
	defer func(start time.Time) { a.Log(request, response, err, time.Since(start)) }(time.Now())
	if err := a.updateMessage(jobInfosTable, request); err != nil {
		return nil, err
	}
	return google_protobuf.EmptyInstance, nil
}

func (a *rethinkAPIServer) CreateJobState(ctx context.Context, request *persist.JobState) (response *google_protobuf.Empty, err error) {
	defer func(start time.Time) { a.Log(request, response, err, time.Since(start)) }(time.Now())
	if err := a.updateMessage(jobInfosTable, request); err != nil {
		return nil, err
	}
	return google_protobuf.EmptyInstance, nil
}

// timestamp cannot be set
func (a *rethinkAPIServer) CreatePipelineInfo(ctx context.Context, request *persist.PipelineInfo) (response *persist.PipelineInfo, err error) {
	defer func(start time.Time) { a.Log(request, response, err, time.Since(start)) }(time.Now())
	if request.CreatedAt != nil {
		return nil, ErrTimestampSet
	}
	request.CreatedAt = a.now()
	if err := a.insertMessage(pipelineInfosTable, request); err != nil {
		return nil, err
	}
	return request, nil
}

func (a *rethinkAPIServer) GetPipelineInfo(ctx context.Context, request *pps.Pipeline) (response *persist.PipelineInfo, err error) {
	defer func(start time.Time) { a.Log(request, response, err, time.Since(start)) }(time.Now())
	pipelineInfo := &persist.PipelineInfo{}
	if err := a.getMessageByPrimaryKey(pipelineInfosTable, request.Name, pipelineInfo); err != nil {
		return nil, err
	}
	return pipelineInfo, nil
}

func (a *rethinkAPIServer) ListPipelineInfos(ctx context.Context, request *google_protobuf.Empty) (response *persist.PipelineInfos, retErr error) {
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	query := a.getTerm(pipelineInfosTable)
	cursor, err := query.Run(a.session)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := cursor.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	result := &persist.PipelineInfos{}
	for {
		pipelineInfo := &persist.PipelineInfo{}
		if !cursor.Next(pipelineInfo) {
			break
		}
		result.PipelineInfo = append(result.PipelineInfo, pipelineInfo)
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (a *rethinkAPIServer) DeletePipelineInfo(ctx context.Context, request *pps.Pipeline) (response *google_protobuf.Empty, err error) {
	defer func(start time.Time) { a.Log(request, response, err, time.Since(start)) }(time.Now())
	if err := a.deleteMessageByPrimaryKey(pipelineInfosTable, request.Name); err != nil {
		return nil, err
	}
	return google_protobuf.EmptyInstance, nil
}

func (a *rethinkAPIServer) insertMessage(table Table, message proto.Message) error {
	_, err := a.getTerm(table).Insert(message).RunWrite(a.session)
	return err
}

func (a *rethinkAPIServer) updateMessage(table Table, message proto.Message) error {
	_, err := a.getTerm(table).Insert(message, gorethink.InsertOpts{Conflict: "update"}).RunWrite(a.session)
	return err
}

func (a *rethinkAPIServer) getMessageByPrimaryKey(table Table, key interface{}, message proto.Message) error {
	cursor, err := a.getTerm(table).Get(key).Default(gorethink.Error("value not found")).Run(a.session)
	if err != nil {
		return err
	}
	if cursor.Next(message) {
		return cursor.Err()
	}
	return nil
}

func (a *rethinkAPIServer) deleteMessageByPrimaryKey(table Table, value interface{}) (retErr error) {
	_, err := a.getTerm(table).Get(value).Delete().RunWrite(a.session)
	return err
}

func (a *rethinkAPIServer) waitMessageByPrimaryKey(
	table Table,
	key interface{},
	message proto.Message,
	predicate func(term gorethink.Term) gorethink.Term,
) (retErr error) {
	term := a.getTerm(table).
		Get(key).
		Default(gorethink.Error("value not found")).
		Changes().
		Field("new_val").
		Filter(predicate)
	cursor, err := term.Run(a.session)
	if err != nil {
		return err
	}
	defer func() {
		if err := cursor.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	cursor.Next(message)
	return cursor.Err()
}

func (a *rethinkAPIServer) getTerm(table Table) gorethink.Term {
	return gorethink.DB(a.databaseName).Table(table)
}

func (a *rethinkAPIServer) now() *google_protobuf.Timestamp {
	return prototime.TimeToTimestamp(a.timer.Now())
}

func genCommitIndex(commits []*pfs.Commit) string {
	var commitIDs []string
	for _, commit := range commits {
		commitIDs = append(commitIDs, commit.Id[0:10])
	}
	sort.Strings(commitIDs)
	var result []byte
	for _, commitID := range commitIDs {
		result = append(result, commitID...)
	}
	return string(result)
}
