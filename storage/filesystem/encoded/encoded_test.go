package encoded_test

import (
	"bytes"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/filesystem/encoded"
)

type testCodec struct{}

func (testCodec) Encode(_ string, plaintext []byte) ([]byte, error) {
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(plaintext)))
	base64.StdEncoding.Encode(encoded, plaintext)
	return append([]byte("encoded:"), encoded...), nil
}

func (testCodec) Decode(_ string, data []byte) ([]byte, error) {
	if !bytes.HasPrefix(data, []byte("encoded:")) {
		return append([]byte(nil), data...), nil
	}
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(data)-len("encoded:")))
	count, err := base64.StdEncoding.Decode(decoded, data[len("encoded:"):])
	return decoded[:count], err
}

func TestFilesystemTempFileSharedReadAndRename(t *testing.T) {
	root, err := encoded.New(osfs.New(t.TempDir()), testCodec{})
	if err != nil {
		t.Fatal(err)
	}
	filesystem, err := root.Chroot(git.GitDirName)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := filesystem.TempFile("objects/pack", "tmp_pack_")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := filesystem.Open(writer.Name())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("pack contents")); err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "pack contents" {
		t.Fatalf("shared contents = %q", contents)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	destination := "objects/pack/pack-test.pack"
	if err := filesystem.Rename(writer.Name(), destination); err != nil {
		t.Fatal(err)
	}
	reopened, err := filesystem.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	contents, err = io.ReadAll(reopened)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if string(contents) != "pack contents" {
		t.Fatalf("reopened contents = %q", contents)
	}
}

func TestFilesystemCloneCommitAndReopen(t *testing.T) {
	source := t.TempDir()
	sourceRepository, err := git.PlainInit(source, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "secret.txt"), []byte("first secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceWorktree, err := sourceRepository.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sourceWorktree.Add("secret.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceWorktree.Commit("initial", &git.CommitOptions{Author: &object.Signature{Name: "Test", Email: "test@example.com"}}); err != nil {
		t.Fatal(err)
	}

	target := t.TempDir()
	worktree, err := encoded.New(osfs.New(target), testCodec{})
	if err != nil {
		t.Fatal(err)
	}
	dotgit, err := worktree.Chroot(git.GitDirName)
	if err != nil {
		t.Fatal(err)
	}
	storage := filesystem.NewStorage(dotgit, cache.NewObjectLRUDefault())
	repository, err := git.Clone(storage, worktree, &git.CloneOptions{URL: source, NoCheckout: true})
	if err != nil {
		t.Fatal(err)
	}
	head, err := repository.Head()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := repository.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("read fetched commit: %v", err)
	}
	if _, err := commit.Tree(); err != nil {
		t.Fatalf("read fetched tree: %v", err)
	}
	clonedWorktree, err := repository.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := clonedWorktree.Reset(&git.ResetOptions{Commit: head.Hash(), Mode: git.HardReset}); err != nil {
		t.Fatalf("checkout fetched commit: %v", err)
	}
	assertEncodedTree(t, target, "first secret")

	file, err := worktree.Create("secret.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("second secret")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	repositoryWorktree, err := repository.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repositoryWorktree.Add("secret.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := repositoryWorktree.Commit("second", &git.CommitOptions{Author: &object.Signature{Name: "Test", Email: "test@example.com"}}); err != nil {
		t.Fatal(err)
	}
	assertEncodedTree(t, target, "second secret")

	reopenedStorage := filesystem.NewStorage(dotgit, cache.NewObjectLRUDefault())
	reopened, err := git.Open(reopenedStorage, worktree)
	if err != nil {
		t.Fatal(err)
	}
	head, err = reopened.Head()
	if err != nil {
		t.Fatal(err)
	}
	commit, err = reopened.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if commit.Message != "second" {
		t.Fatalf("commit message = %q", commit.Message)
	}
	opened, err := worktree.Open("secret.txt")
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := io.ReadAll(opened)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "second secret" {
		t.Fatalf("worktree content = %q", plaintext)
	}
}

func assertEncodedTree(t *testing.T, root string, plaintext string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(data) == 0 || !bytes.HasPrefix(data, []byte("encoded:")) {
			t.Fatalf("%s is not encoded", path)
		}
		if bytes.Contains(data, []byte(plaintext)) {
			t.Fatalf("%s contains plaintext", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
