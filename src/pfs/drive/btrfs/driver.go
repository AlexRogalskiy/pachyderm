/*

directory structure

  .
  |-- repositoryName
	  |-- scratch
		  |-- shardNum // the read-only read created on InitRepository, this is where to start branching
      |-- commitID
	      |-- shardNum // this is where subvolumes are

*/
package btrfs

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pachyderm/pachyderm/src/pfs"
	"github.com/pachyderm/pachyderm/src/pfs/drive"
	"github.com/pachyderm/pachyderm/src/pkg/executil"
	"github.com/peter-edge/go-google-protobuf"
)

type driver struct {
	rootDir string
}

func newDriver(rootDir string) *driver {
	return &driver{rootDir}
}

func (d *driver) Init() error {
	return nil
}

func (d *driver) InitRepository(repository *pfs.Repository, shards map[int]bool) error {
	// syscall.Mkdir (which os.Mkdir directly calls) is atomic across processes, and since
	// we do not include shards as part of the repository path, this guaranteees
	// only one initialization will complete successfully
	if err := os.Mkdir(d.repositoryPath(repository), 0700); err != nil {
		return err
	}
	initialCommit := &pfs.Commit{
		Repository: repository,
		Id:         drive.InitialCommitID,
	}
	if err := os.Mkdir(d.commitPathNoShard(initialCommit), 0700); err != nil {
		return err
	}
	for shard := range shards {
		if err := subvolumeCreate(d.commitPath(initialCommit, shard)); err != nil {
			return err
		}
		if err := d.initMeta(initialCommit, shard, ""); err != nil {
			return err
		}
		if err := setReadOnly(d.commitPath(initialCommit, shard)); err != nil {
			return err
		}
	}
	return nil
}

func (d *driver) GetFile(path *pfs.Path, shard int) (io.ReadCloser, error) {
	return os.Open(d.filePath(path, shard))
}

func (d *driver) MakeDirectory(path *pfs.Path, shards map[int]bool) error {
	// TODO(pedge): if PutFile fails here or on another shard, the directories
	// will still exist and be returned from ListFiles, we want to do this
	// iteratively and with rollback
	// TODO(pedge): check that commit exists and is a write commit
	for shard := range shards {
		if err := os.MkdirAll(d.filePath(path, shard), 0700); err != nil {
			return err
		}
	}
	return nil
}

func (d *driver) PutFile(path *pfs.Path, shard int, reader io.Reader) error {
	file, err := os.Create(d.filePath(path, shard))
	if err != nil {
		return err
	}
	_, err = bufio.NewReader(reader).WriteTo(file)
	return err
}

func (d *driver) ListFiles(path *pfs.Path, shard int) (retValue []*pfs.FileInfo, retErr error) {
	filePath := d.filePath(path, shard)
	stat, err := os.Stat(filePath)
	if err != nil {
		return nil, err
	}
	if !stat.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", filePath)
	}
	dir, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := dir.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	var fileInfos []*pfs.FileInfo
	// TODO(pedge): constant
	for names, err := dir.Readdirnames(100); err != io.EOF; names, err = dir.Readdirnames(100) {
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			subPath := &pfs.Path{
				Commit: path.Commit,
				Path:   filepath.Join(path.Path, name),
			}
			subFilePath := d.filePath(path, shard)
			stat, err := os.Stat(subFilePath)
			if err != nil {
				return nil, err
			}
			fileType := pfs.FileType_FILE_TYPE_OTHER
			if stat.Mode().IsRegular() {
				fileType = pfs.FileType_FILE_TYPE_REGULAR
			}
			if stat.Mode().IsDir() {
				fileType = pfs.FileType_FILE_TYPE_DIR
			}
			fileInfos = append(
				fileInfos,
				&pfs.FileInfo{
					Path:      subPath,
					FileType:  fileType,
					SizeBytes: uint64(stat.Size()),
					Perm:      uint32(stat.Mode() & os.ModePerm),
					LastModified: &google_protobuf.Timestamp{
						Seconds: stat.ModTime().UnixNano() / int64(time.Second),
						Nanos:   int32(stat.ModTime().UnixNano() % int64(time.Second)),
					},
				},
			)
		}
	}
	return fileInfos, nil
}

func (d *driver) Branch(commit *pfs.Commit, newCommit *pfs.Commit, shards map[int]bool) (*pfs.Commit, error) {
	if newCommit == nil {
		newCommit = &pfs.Commit{
			Repository: commit.Repository,
			Id:         drive.NewCommitID(),
		}
	}
	if err := os.Mkdir(d.commitPathNoShard(newCommit), 0700); err != nil {
		return nil, err
	}
	for shard := range shards {
		commitPath := d.commitPath(commit, shard)
		readOnly, err := isReadOnly(commitPath)
		if err != nil {
			return nil, err
		}
		if !readOnly {
			return nil, fmt.Errorf("%+v is not a read-only snapshot", commit)
		}
		newCommitPath := d.commitPath(newCommit, shard)
		if err := subvolumeSnapshot(commitPath, newCommitPath); err != nil {
			return nil, err
		}
		if err := d.initMeta(newCommit, shard, commit.Id); err != nil {
			return nil, err
		}
	}
	return newCommit, nil
}

func (d *driver) Commit(commit *pfs.Commit, shards map[int]bool) error {
	for shard := range shards {
		commitPath := d.commitPath(commit, shard)
		readOnly, err := isReadOnly(commitPath)
		if err != nil {
			return err
		}
		if readOnly {
			return fmt.Errorf("%+v is a read-only snapshot", commit)
		}
		if err := setReadOnly(commitPath); err != nil {
			return err
		}
	}
	return nil
}

func (d *driver) PullDiff(commit *pfs.Commit, shard int) (io.Reader, error) {
	return nil, nil
}

func (d *driver) PushDiff(commit *pfs.Commit, shard int, reader io.Reader) error {
	return nil
}

func (d *driver) GetCommitInfo(commit *pfs.Commit, shard int) (*pfs.CommitInfo, error) {
	parent, err := d.getParent(commit, shard)
	if err != nil {
		return nil, err
	}
	readOnly, err := isReadOnly(d.commitPath(commit, shard))
	if err != nil {
		return nil, err
	}
	commitType := pfs.CommitType_COMMIT_TYPE_WRITE
	if readOnly {
		commitType = pfs.CommitType_COMMIT_TYPE_READ
	}
	return &pfs.CommitInfo{
		Commit:       commit,
		CommitType:   commitType,
		ParentCommit: parent,
	}, nil
}

func (d *driver) initMeta(commit *pfs.Commit, shard int, parentCommitID string) (retErr error) {
	if commit.Id == drive.InitialCommitID {
		if err := os.Mkdir(
			d.filePath(
				&pfs.Path{
					Commit: commit,
					Path:   ".pfs",
				},
				shard,
			),
			0700,
		); err != nil {
			return err
		}
	}
	if parentCommitID == "" {
		if commit.Id != drive.InitialCommitID {
			return fmt.Errorf("no parent commit id for %s", commit.Id)
		}
		return nil
	}
	parentFile, err := os.Create(
		d.filePath(
			&pfs.Path{
				Commit: commit,
				Path:   ".pfs/parent",
			},
			shard,
		),
	)
	if err != nil {
		return err
	}
	defer func() {
		if err := parentFile.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	_, err = parentFile.Write([]byte(parentCommitID))
	return err
}

func (d *driver) getParent(commit *pfs.Commit, shard int) (*pfs.Commit, error) {
	if commit.Id == drive.InitialCommitID {
		return nil, nil
	}
	data, err := ioutil.ReadFile(
		d.filePath(
			&pfs.Path{
				Commit: commit,
				Path:   ".pfs/parent",
			},
			shard,
		),
	)
	if err != nil {
		return nil, err
	}
	return &pfs.Commit{
		Repository: commit.Repository,
		Id:         string(data),
	}, nil
}

func (d *driver) repositoryPath(repository *pfs.Repository) string {
	return filepath.Join(d.rootDir, repository.Name)
}

func (d *driver) commitPathNoShard(commit *pfs.Commit) string {
	return filepath.Join(d.repositoryPath(commit.Repository), commit.Id)
}

func (d *driver) commitPath(commit *pfs.Commit, shard int) string {
	return filepath.Join(d.commitPathNoShard(commit), fmt.Sprintf("%d", shard))
}

func (d *driver) filePath(path *pfs.Path, shard int) string {
	return filepath.Join(d.commitPath(path.Commit, shard), path.Path)
}

func isReadOnly(path string) (bool, error) {
	reader, err := snapshotPropertyGet(path)
	if err != nil {
		return false, err
	}
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		text := scanner.Text()
		if strings.Contains(text, "ro=true") {
			return true, nil
		}
		if strings.Contains(text, "ro=false") {
			return false, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return false, errors.New("did not fins ro=true or ro=false in output")
}

func setReadOnly(path string) error {
	return snapshotPropertySet(path, "ro", "true")
}

func snapshotPropertyGet(path string) (io.Reader, error) {
	return executil.RunStdout("btrfs", "property", "get", "-t", "s", path)
}

func snapshotPropertySet(path string, key string, value string) error {
	return executil.Run("btrfs", "property", "set", "-t", "s", path, key, value)
}

func subvolumeCreate(path string) error {
	return executil.Run("btrfs", "subvolume", "create", path)
}

func subvolumeSnapshot(src string, dest string) error {
	return executil.Run("btrfs", "subvolume", "snapshot", src, dest)
}

func subvolumeSnapshotReadonly(src string, dest string) error {
	return executil.Run("btrfs", "subvolume", "snapshot", "-r", src, dest)
}
