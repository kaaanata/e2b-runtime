package blockvol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// fakeRunner emulates the subset of btrfs-progs and e2fsprogs the backend
// calls, on an ordinary directory tree, so backend sequencing, idempotency
// and crash-resume can be tested without a Btrfs mount (and on macOS).
//
//   - A "subvolume" is a directory holding a .fakesubvol JSON record.
//   - A snapshot is a recursive copy with a fresh record.
//   - The ext4 "superblock" is the 36-byte UUID string at offset 1024 of
//     the image file, so snapshots carry it exactly like real images do.
type fakeRunner struct {
	mu    sync.Mutex
	usage []byte
	calls []string
	// failOn makes the n-th (1-based) invocation whose "name sub" key
	// matches fail, simulating a crash or tool error at that step.
	failOn map[string]int
	seen   map[string]int
}

const (
	fakeSubvolRecord = ".fakesubvol"
	fakeSBOffset     = 1024
)

type fakeSubvol struct {
	UUID       string `json:"uuid"`
	ParentUUID string `json:"parent_uuid"`
	ReadOnly   bool   `json:"readonly"`
}

func newFakeRunner(usage []byte) *fakeRunner {
	return &fakeRunner{usage: usage, failOn: map[string]int{}, seen: map[string]int{}}
}

func (f *fakeRunner) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0
	for _, c := range f.calls {
		if c == key {
			n++
		}
	}

	return n
}

func (f *fakeRunner) failNext(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOn[key] = f.seen[key] + 1
}

func cmdKey(name string, args []string) string {
	if name == "btrfs" && len(args) >= 2 {
		return name + " " + args[0] + " " + args[1]
	}

	return name
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := cmdKey(name, args)

	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.seen[key]++
	inject := f.failOn[key] == f.seen[key]
	f.mu.Unlock()

	if inject {
		return nil, &CommandError{Name: name, Args: args, ExitCode: 1, Stderr: "injected failure", Err: errors.New("exit status 1")}
	}

	out, err := f.exec(name, args)
	if err != nil {
		return nil, &CommandError{Name: name, Args: args, ExitCode: 1, Stderr: err.Error(), Err: err}
	}

	return out, nil
}

func (f *fakeRunner) exec(name string, args []string) ([]byte, error) {
	last := args[len(args)-1]

	switch key := cmdKey(name, args); key {
	case "btrfs subvolume create":
		if err := os.Mkdir(last, 0o700); err != nil {
			return nil, err
		}

		return nil, writeFakeSubvol(last, fakeSubvol{UUID: uuid.NewString()})
	case "btrfs subvolume snapshot":
		ro := args[2] == "-r"
		src, dst := args[len(args)-2], last

		parent, err := readFakeSubvol(src)
		if err != nil {
			return nil, err
		}

		if _, err := os.Lstat(dst); err == nil {
			return nil, fmt.Errorf("ERROR: target path already exists: %s", dst)
		}

		if err := copyTree(src, dst); err != nil {
			return nil, err
		}

		return nil, writeFakeSubvol(dst, fakeSubvol{UUID: uuid.NewString(), ParentUUID: parent.UUID, ReadOnly: ro})
	case "btrfs subvolume delete":
		if _, err := readFakeSubvol(last); err != nil {
			return nil, fmt.Errorf("ERROR: not a subvolume: %s", last)
		}

		return nil, os.RemoveAll(last)
	case "btrfs subvolume show":
		sv, err := readFakeSubvol(last)
		if err != nil {
			return nil, err
		}

		parent, flags := "-", "-"
		if sv.ParentUUID != "" {
			parent = sv.ParentUUID
		}

		if sv.ReadOnly {
			flags = "readonly"
		}

		return fmt.Appendf(nil, "%s\n\tName: \t\t\t%s\n\tUUID: \t\t\t%s\n\tParent UUID: \t\t%s\n\tSubvolume ID: \t\t256\n\tGeneration: \t\t7\n\tFlags: \t\t\t%s\n\tSnapshot(s):\n", last, filepath.Base(last), sv.UUID, parent, flags), nil
	case "btrfs filesystem usage":
		return f.usage, nil
	case "fallocate":
		size, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return nil, err
		}

		file, err := os.OpenFile(last, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		defer file.Close()

		return nil, file.Truncate(size)
	case "mkfs.ext4":
		for i, a := range args {
			if a == "-U" {
				return nil, writeFakeSB(last, args[i+1])
			}
		}

		return nil, errors.New("mkfs.ext4 without -U")
	case "tune2fs":
		return nil, writeFakeSB(last, uuid.NewString())
	case "e2fsck":
		return nil, nil
	case "blkid":
		raw := make([]byte, 36)

		file, err := os.Open(last)
		if err != nil {
			return nil, err
		}
		defer file.Close()

		if _, err := file.ReadAt(raw, fakeSBOffset); err != nil {
			return nil, err
		}

		return []byte(string(raw) + "\n"), nil
	default:
		return nil, fmt.Errorf("fakeRunner: unsupported command %q", key)
	}
}

func writeFakeSubvol(dir string, sv fakeSubvol) error {
	raw, err := json.Marshal(sv)
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dir, fakeSubvolRecord), raw, 0o600)
}

func readFakeSubvol(dir string) (fakeSubvol, error) {
	var sv fakeSubvol

	raw, err := os.ReadFile(filepath.Join(dir, fakeSubvolRecord))
	if err != nil {
		return sv, fmt.Errorf("ERROR: not a subvolume: %s: %w", dir, err)
	}

	return sv, json.Unmarshal(raw, &sv)
}

func writeFakeSB(image, id string) error {
	file, err := os.OpenFile(image, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = file.WriteAt([]byte(id), fakeSBOffset)

	return err
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}

		if strings.HasSuffix(path, fakeSubvolRecord) {
			return nil
		}

		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()

		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer out.Close()

		_, err = io.Copy(out, in)

		return err
	})
}
