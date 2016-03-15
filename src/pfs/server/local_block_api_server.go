package server

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/golang/protobuf/proto"
	pfsserver "github.com/pachyderm/pachyderm/src/pfs"
	pfsclient "github.com/pachyderm/pachyderm/src/client/pfs"
	"go.pedge.io/pb/go/google/protobuf"
	"go.pedge.io/proto/rpclog"
	"go.pedge.io/proto/stream"
	"go.pedge.io/proto/time"
	"golang.org/x/net/context"
)

type localBlockAPIServer struct {
	protorpclog.Logger
	dir string
}

func newLocalBlockAPIServer(dir string) (*localBlockAPIServer, error) {
	server := &localBlockAPIServer{
		Logger: protorpclog.NewLogger("pachyderm.pfsserver.localBlockAPIServer"),
		dir:    dir,
	}
	if err := os.MkdirAll(server.tmpDir(), 0777); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(server.diffDir(), 0777); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(server.blockDir(), 0777); err != nil {
		return nil, err
	}
	return server, nil
}

func (s *localBlockAPIServer) PutBlock(putBlockServer pfsclient.BlockAPI_PutBlockServer) (retErr error) {
	result := &pfsserver.BlockRefs{}
	defer func(start time.Time) { s.Log(nil, result, retErr, time.Since(start)) }(time.Now())
	scanner := bufio.NewScanner(protostream.NewStreamingBytesReader(putBlockServer))
	for {
		blockRef, err := s.putOneBlock(scanner)
		if err != nil {
			return err
		}
		result.BlockRef = append(result.BlockRef, blockRef)
		if (blockRef.Range.Upper - blockRef.Range.Lower) < uint64(blockSize) {
			break
		}
	}
	return putBlockServer.SendAndClose(result)
}

func (s *localBlockAPIServer) blockFile(block *pfsserver.Block) (*os.File, error) {
	return os.Open(s.blockPath(block))
}

func (s *localBlockAPIServer) GetBlock(request *pfsclient.GetBlockRequest, getBlockServer pfsclient.BlockAPI_GetBlockServer) (retErr error) {
	defer func(start time.Time) { s.Log(request, nil, retErr, time.Since(start)) }(time.Now())
	file, err := s.blockFile(request.Block)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	reader := io.NewSectionReader(file, int64(request.OffsetBytes), int64(request.SizeBytes))
	return protostream.WriteToStreamingBytesServer(reader, getBlockServer)
}

func (s *localBlockAPIServer) DeleteBlock(ctx context.Context, request *pfsclient.DeleteBlockRequest) (response *google_protobuf.Empty, retErr error) {
	defer func(start time.Time) { s.Log(request, response, retErr, time.Since(start)) }(time.Now())
	return google_protobuf.EmptyInstance, s.deleteBlock(request.Block)
}

func (s *localBlockAPIServer) InspectBlock(ctx context.Context, request *pfsclient.InspectBlockRequest) (response *pfsserver.BlockInfo, retErr error) {
	defer func(start time.Time) { s.Log(request, response, retErr, time.Since(start)) }(time.Now())
	stat, err := os.Stat(s.blockPath(request.Block))
	if err != nil {
		return nil, err
	}
	return &pfsserver.BlockInfo{
		Block: request.Block,
		Created: prototime.TimeToTimestamp(
			stat.ModTime(),
		),
		SizeBytes: uint64(stat.Size()),
	}, nil
}

func (s *localBlockAPIServer) ListBlock(ctx context.Context, request *pfsclient.ListBlockRequest) (response *pfsserver.BlockInfos, retErr error) {
	defer func(start time.Time) { s.Log(request, response, retErr, time.Since(start)) }(time.Now())
	return nil, fmt.Errorf("not implemented")
}

func (s *localBlockAPIServer) CreateDiff(ctx context.Context, request *pfsserver.DiffInfo) (response *google_protobuf.Empty, retErr error) {
	defer func(start time.Time) { s.Log(request, response, retErr, time.Since(start)) }(time.Now())
	data, err := proto.Marshal(request)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(path.Dir(s.diffPath(request.Diff)), 0777); err != nil {
		return nil, err
	}
	if err := ioutil.WriteFile(s.diffPath(request.Diff), data, 0666); err != nil {
		return nil, err
	}
	return google_protobuf.EmptyInstance, nil
}

func (s *localBlockAPIServer) InspectDiff(ctx context.Context, request *pfsclient.InspectDiffRequest) (response *pfsserver.DiffInfo, retErr error) {
	defer func(start time.Time) { s.Log(request, response, retErr, time.Since(start)) }(time.Now())
	return s.readDiff(request.Diff)
}

func (s *localBlockAPIServer) ListDiff(request *pfsclient.ListDiffRequest, listDiffServer pfsclient.BlockAPI_ListDiffServer) (retErr error) {
	defer func(start time.Time) { s.Log(request, nil, retErr, time.Since(start)) }(time.Now())
	if err := filepath.Walk(s.diffDir(), func(path string, info os.FileInfo, err error) error {
		diff := s.pathToDiff(path)
		if diff == nil {
			// likely a directory
			return nil
		}
		if diff.Shard == request.Shard {
			diffInfo, err := s.readDiff(diff)
			if err != nil {
				return err
			}
			if err := listDiffServer.Send(diffInfo); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (s *localBlockAPIServer) DeleteDiff(ctx context.Context, request *pfsclient.DeleteDiffRequest) (response *google_protobuf.Empty, retErr error) {
	defer func(start time.Time) { s.Log(request, response, retErr, time.Since(start)) }(time.Now())
	return google_protobuf.EmptyInstance, os.Remove(s.diffPath(request.Diff))
}

func (s *localBlockAPIServer) tmpDir() string {
	return filepath.Join(s.dir, "tmp")
}

func (s *localBlockAPIServer) blockDir() string {
	return filepath.Join(s.dir, "block")
}

func (s *localBlockAPIServer) blockPath(block *pfsserver.Block) string {
	return filepath.Join(s.blockDir(), block.Hash)
}

func (s *localBlockAPIServer) diffDir() string {
	return filepath.Join(s.dir, "diff")
}

func (s *localBlockAPIServer) diffPath(diff *pfsserver.Diff) string {
	return filepath.Join(s.diffDir(), diff.Commit.Repo.Name, diff.Commit.ID, strconv.FormatUint(diff.Shard, 10))
}

// pathToDiff parses a path as a diff, it returns nil when parse fails
func (s *localBlockAPIServer) pathToDiff(path string) *pfsserver.Diff {
	repoCommitShard := strings.Split(strings.TrimPrefix(path, s.diffDir()), "/")
	if len(repoCommitShard) < 3 {
		return nil
	}
	shard, err := strconv.ParseUint(repoCommitShard[2], 10, 64)
	if err != nil {
		return nil
	}
	return &pfsserver.Diff{
		Commit: &pfsserver.Commit{
			Repo: &pfsserver.Repo{Name: repoCommitShard[0]},
			ID:   repoCommitShard[1],
		},
		Shard: shard,
	}
}

func (s *localBlockAPIServer) readDiff(diff *pfsserver.Diff) (*pfsserver.DiffInfo, error) {
	data, err := ioutil.ReadFile(s.diffPath(diff))
	if err != nil {
		return nil, err
	}
	result := &pfsserver.DiffInfo{}
	if err := proto.Unmarshal(data, result); err != nil {
		return nil, err
	}
	return result, nil
}

func scanBlock(scanner *bufio.Scanner) (*pfsserver.BlockRef, []byte, error) {
	var buffer bytes.Buffer
	var bytesWritten int
	hash := newHash()
	for scanner.Scan() {
		// they take out the newline, put it back
		bytes := append(scanner.Bytes(), '\n')
		buffer.Write(bytes)
		hash.Write(bytes)
		bytesWritten += len(bytes)
		if bytesWritten > blockSize {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	return &pfsserver.BlockRef{
		Block: getBlock(hash),
		Range: &pfsserver.ByteRange{
			Lower: 0,
			Upper: uint64(buffer.Len()),
		},
	}, buffer.Bytes(), nil
}

func (s *localBlockAPIServer) putOneBlock(scanner *bufio.Scanner) (*pfsserver.BlockRef, error) {
	blockRef, data, err := scanBlock(scanner)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(s.blockPath(blockRef.Block)); os.IsNotExist(err) {
		ioutil.WriteFile(s.blockPath(blockRef.Block), data, 0666)
	}
	return blockRef, nil
}

func (s *localBlockAPIServer) deleteBlock(block *pfsserver.Block) error {
	return os.Remove(s.blockPath(block))
}
