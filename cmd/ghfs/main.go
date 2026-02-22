package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	github "github.com/google/go-github/v62/github"
)

const (
	exitUsage    = 2
	exitPreflight = 3
	exitMount    = 4
	exitReady    = 5
	exitUnmount  = 6
)

type mountState struct {
	Mountpoint string `json:"mountpoint"`
	PID        int    `json:"pid"`
	StartedAt  string `json:"started_at"`
	TokenSource string `json:"token_source"`
}

func main() {
	log.SetFlags(0)
	if len(os.Args) <= 1 {
		legacyMount(os.Args[1:])
		return
	}

	switch os.Args[1] {
	case "mount":
		runMount(os.Args[2:])
	case "doctor":
		runDoctor()
	case "status":
		runStatus(os.Args[2:])
	case "unmount":
		runUnmount(os.Args[2:])
	case "-h", "--help", "help":
		printUsage()
	default:
		// Backward compatibility: ghfs <mountpoint> [--token ...]
		legacyMount(os.Args[1:])
	}
}

func printUsage() {
	fmt.Println("ghfs usage:")
	fmt.Println("  ghfs mount [--token <token>] [--token-file <file>] [--token-source env|none] <mountpoint>")
	fmt.Println("  ghfs doctor")
	fmt.Println("  ghfs status <mountpoint>")
	fmt.Println("  ghfs unmount <mountpoint>")
	fmt.Println("  ghfs <mountpoint> [--token <token>]   # legacy")
}

func legacyMount(args []string) {
	fs := flag.NewFlagSet("ghfs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	token := fs.String("token", "", "personal access token")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR: invalid args")
		printUsage()
		os.Exit(exitUsage)
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "ERROR: mountpoint required")
		printUsage()
		os.Exit(exitUsage)
	}
	mountPath := fs.Arg(0)
	serveMount(mountPath, *token, tokenSource(*token, "", ""))
}

func runMount(args []string) {
	fs := flag.NewFlagSet("mount", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	token := fs.String("token", "", "GitHub token")
	tokenFile := fs.String("token-file", "", "Path to token file")
	tokenSrc := fs.String("token-source", "env", "env|none")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR: invalid mount args")
		printUsage()
		os.Exit(exitUsage)
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "ERROR: mountpoint required")
		os.Exit(exitUsage)
	}
	mountPath := fs.Arg(0)
	resolvedToken := *token
	if resolvedToken == "" && *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: unable to read token file: %v\n", err)
			os.Exit(exitUsage)
		}
		resolvedToken = strings.TrimSpace(string(b))
	}
	if resolvedToken == "" && *tokenSrc == "env" {
		resolvedToken = strings.TrimSpace(os.Getenv("GHFS_GITHUB_TOKEN"))
	}
	serveMount(mountPath, resolvedToken, tokenSource(*token, *tokenFile, *tokenSrc))
}

func tokenSource(token, tokenFile, tokenSrc string) string {
	if token != "" {
		return "flag"
	}
	if tokenFile != "" {
		return "token-file"
	}
	if tokenSrc == "env" {
		if os.Getenv("GHFS_GITHUB_TOKEN") != "" {
			return "env"
		}
		return "env-empty"
	}
	return tokenSrc
}

func runDoctor() {
	issues := make([]string, 0)
	infos := make([]string, 0)

	if _, err := os.Stat("/dev/fuse"); err != nil {
		issues = append(issues, "missing /dev/fuse (FUSE unavailable)")
	} else {
		infos = append(infos, "/dev/fuse present")
	}
	if _, err := exec.LookPath("fusermount"); err != nil {
		if _, err2 := exec.LookPath("umount"); err2 != nil {
			issues = append(issues, "missing fusermount and umount")
		} else {
			infos = append(infos, "umount present (fusermount missing)")
		}
	} else {
		infos = append(infos, "fusermount present")
	}
	if strings.TrimSpace(os.Getenv("GHFS_GITHUB_TOKEN")) == "" {
		issues = append(issues, "GHFS_GITHUB_TOKEN is unset (mount may hit rate limits)")
	} else {
		infos = append(infos, "GHFS_GITHUB_TOKEN set")
	}

	for _, info := range infos {
		fmt.Printf("[ghfs:doctor] OK: %s\n", info)
	}
	if len(issues) > 0 {
		for _, issue := range issues {
			fmt.Printf("[ghfs:doctor] WARN: %s\n", issue)
		}
		if hasFatalPreflight(issues) {
			os.Exit(exitPreflight)
		}
	}
	fmt.Println("[ghfs:doctor] done")
}

func hasFatalPreflight(issues []string) bool {
	for _, issue := range issues {
		if strings.Contains(issue, "missing /dev/fuse") || strings.Contains(issue, "missing fusermount and umount") {
			return true
		}
	}
	return false
}

func runStatus(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "ERROR: status requires <mountpoint>")
		os.Exit(exitUsage)
	}
	mountpoint := args[0]
	mounted := isMountedFuse(mountpoint)
	state, _ := readState(mountpoint)

	fmt.Printf("mountpoint: %s\n", mountpoint)
	fmt.Printf("mounted: %t\n", mounted)
	if state != nil {
		fmt.Printf("pid: %d\n", state.PID)
		fmt.Printf("started_at: %s\n", state.StartedAt)
		fmt.Printf("token_source: %s\n", state.TokenSource)
		if processExists(state.PID) {
			fmt.Println("process_alive: true")
		} else {
			fmt.Println("process_alive: false")
		}
	} else {
		fmt.Println("state: none")
	}
	if mounted {
		os.Exit(0)
	}
	os.Exit(1)
}

func runUnmount(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "ERROR: unmount requires <mountpoint>")
		os.Exit(exitUsage)
	}
	mountpoint := args[0]
	if !isMountedFuse(mountpoint) {
		_ = clearState(mountpoint)
		fmt.Printf("[ghfs:unmount] already unmounted: %s\n", mountpoint)
		return
	}
	if err := unmountPath(mountpoint); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: unmount failed: %v\n", err)
		os.Exit(exitUnmount)
	}
	_ = clearState(mountpoint)
	fmt.Printf("[ghfs:unmount] done: %s\n", mountpoint)
}

func serveMount(mountPath, token, tokenSrc string) {
	if strings.TrimSpace(mountPath) == "" {
		fmt.Fprintln(os.Stderr, "ERROR: mountpoint required")
		os.Exit(exitUsage)
	}
	if err := os.MkdirAll(mountPath, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: cannot create mountpoint: %v\n", err)
		os.Exit(exitMount)
	}

	log.Printf("mounting to: %s", mountPath)
	conn, err := fuse.Mount(mountPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: fuse mount failed: %v\n", err)
		os.Exit(exitMount)
	}
	defer conn.Close()

	_ = writeState(mountPath, &mountState{Mountpoint: mountPath, PID: os.Getpid(), StartedAt: time.Now().UTC().Format(time.RFC3339), TokenSource: tokenSrc})
	defer clearState(mountPath)

	client := newGitHubClient(token)
	filesys := &FS{Client: client}
	if err := fs.Serve(conn, filesys); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: serving fuse fs failed: %v\n", err)
		os.Exit(exitMount)
	}
}

func newGitHubClient(token string) *github.Client {
	if token == "" {
		return github.NewClient(nil)
	}
	httpClient := &http.Client{Transport: &tokenTransport{Token: token, Base: http.DefaultTransport}}
	return github.NewClient(httpClient)
}

type tokenTransport struct {
	Token string
	Base  http.RoundTripper
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "token "+t.Token)
	return base.RoundTrip(clone)
}

type FS struct {
	Client *github.Client
}

func (f *FS) Root() (fs.Node, error) {
	return &Root{FS: f}, nil
}

type Root struct {
	FS *FS
}

func (r *Root) Attr(_ context.Context, attr *fuse.Attr) error {
	attr.Mode = os.ModeDir | 0o755
	return nil
}

func (r *Root) Lookup(ctx context.Context, name string) (fs.Node, error) {
	u, _, err := r.FS.Client.Users.Get(ctx, name)
	if err != nil {
		return nil, mapGitHubErr(err)
	}
	return &User{FS: r.FS, User: u}, nil
}

type User struct {
	*github.User
	FS *FS
}

func (u *User) Attr(_ context.Context, attr *fuse.Attr) error {
	attr.Mode = os.ModeDir | 0o755
	return nil
}

func (u *User) Lookup(ctx context.Context, name string) (fs.Node, error) {
	repo, _, err := u.FS.Client.Repositories.Get(ctx, u.GetLogin(), name)
	if err != nil {
		return nil, mapGitHubErr(err)
	}
	return &Repository{FS: u.FS, Repository: repo}, nil
}

type Repository struct {
	*github.Repository
	FS *FS
}

var _ fs.HandleReadDirAller = (*Repository)(nil)

func (r *Repository) Attr(_ context.Context, attr *fuse.Attr) error {
	attr.Mode = os.ModeDir | 0o755
	return nil
}

func (r *Repository) Lookup(ctx context.Context, name string) (fs.Node, error) {
	owner, repo := r.GetOwner().GetLogin(), r.GetName()
	return lookupPath(ctx, r.FS, owner, repo, name)
}

func (r *Repository) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	owner, repo := r.GetOwner().GetLogin(), r.GetName()
	return listDirectory(ctx, r.FS, owner, repo, "")
}

type File struct {
	FS      *FS
	Content *github.RepositoryContent
}

func (f *File) Attr(_ context.Context, attr *fuse.Attr) error {
	mode := os.FileMode(0o444)
	if f.Content.GetType() == "symlink" {
		mode = os.FileMode(0o777)
	}
	attr.Mode = mode
	attr.Size = uint64(f.Content.GetSize())
	return nil
}

var _ fs.NodeOpener = (*File)(nil)

func (f *File) Open(_ context.Context, _ *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	resp.Flags |= fuse.OpenNonSeekable

	decoded, err := f.Content.GetContent()
	if err == nil && decoded != "" {
		return &FileHandle{r: strings.NewReader(decoded)}, nil
	}

	downloadURL := f.Content.GetDownloadURL()
	if downloadURL == "" {
		return nil, fuse.ENOENT
	}
	httpResp, httpErr := f.FS.Client.Client().Get(downloadURL)
	if httpErr != nil {
		return nil, fuse.EIO
	}
	return &FileHandle{r: httpResp.Body}, nil
}

type FileHandle struct {
	r io.Reader
}

var _ fs.HandleReader = (*FileHandle)(nil)

func (fh *FileHandle) Read(_ context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	buf := make([]byte, req.Size)
	n, err := fh.r.Read(buf)
	resp.Data = buf[:n]
	if err == io.EOF {
		return nil
	}
	return err
}

type Dir struct {
	FS    *FS
	Owner string
	Repo  string
	Path  string
}

var _ fs.HandleReadDirAller = (*Dir)(nil)

func (d *Dir) Attr(_ context.Context, attr *fuse.Attr) error {
	attr.Mode = os.ModeDir | 0o755
	return nil
}

func (d *Dir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	path := name
	if d.Path != "" {
		path = d.Path + "/" + name
	}
	return lookupPath(ctx, d.FS, d.Owner, d.Repo, path)
}

func (d *Dir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	return listDirectory(ctx, d.FS, d.Owner, d.Repo, d.Path)
}

func lookupPath(ctx context.Context, fsState *FS, owner, repo, path string) (fs.Node, error) {
	fileContent, directoryContent, _, err := fsState.Client.Repositories.GetContents(ctx, owner, repo, path, nil)
	if err != nil {
		return nil, mapGitHubErr(err)
	}
	if fileContent != nil {
		return &File{FS: fsState, Content: fileContent}, nil
	}
	if directoryContent != nil {
		return &Dir{FS: fsState, Owner: owner, Repo: repo, Path: path}, nil
	}
	return nil, fuse.ENOENT
}

func listDirectory(ctx context.Context, fsState *FS, owner, repo, path string) ([]fuse.Dirent, error) {
	_, directoryContent, _, err := fsState.Client.Repositories.GetContents(ctx, owner, repo, path, nil)
	if err != nil {
		return nil, mapGitHubErr(err)
	}

	entries := make([]fuse.Dirent, 0, len(directoryContent))
	for _, entry := range directoryContent {
		name := entry.GetName()
		if name == "" {
			continue
		}
		direntType := fuse.DT_Unknown
		switch entry.GetType() {
		case "dir":
			direntType = fuse.DT_Dir
		case "file", "symlink", "submodule":
			direntType = fuse.DT_File
		}
		entries = append(entries, fuse.Dirent{Name: name, Type: direntType})
	}
	return entries, nil
}

func mapGitHubErr(err error) error {
	var rerr *github.RateLimitError
	if errors.As(err, &rerr) {
		return syscall.EAGAIN
	}
	var aerr *github.AbuseRateLimitError
	if errors.As(err, &aerr) {
		return syscall.EAGAIN
	}
	var respErr *github.ErrorResponse
	if errors.As(err, &respErr) {
		switch respErr.Response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return syscall.EACCES
		case http.StatusNotFound:
			return fuse.ENOENT
		case http.StatusTooManyRequests:
			return syscall.EAGAIN
		default:
			return fuse.EIO
		}
	}
	return fuse.EIO
}

func isMountedFuse(mountpoint string) bool {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	needle := " " + mountpoint + " "
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		if strings.Contains(line, " fuse") || strings.Contains(line, "fuse.") {
			return true
		}
	}
	return false
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil
}

func stateDir() string {
	return filepath.Join(os.TempDir(), "ghfs-state")
}

func statePath(mountpoint string) string {
	safe := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(filepath.Clean(mountpoint))
	return filepath.Join(stateDir(), safe+".json")
}

func writeState(mountpoint string, state *mountState) error {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(mountpoint), b, 0o644)
}

func readState(mountpoint string) (*mountState, error) {
	b, err := os.ReadFile(statePath(mountpoint))
	if err != nil {
		return nil, err
	}
	var st mountState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func clearState(mountpoint string) error {
	if err := os.Remove(statePath(mountpoint)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func unmountPath(mountpoint string) error {
	if p, err := exec.LookPath("fusermount"); err == nil {
		cmd := exec.Command(p, "-u", mountpoint)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	if p, err := exec.LookPath("umount"); err == nil {
		cmd := exec.Command(p, mountpoint)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	return fmt.Errorf("no unmount command found")
}
