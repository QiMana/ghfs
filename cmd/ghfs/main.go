package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	github "github.com/google/go-github/v62/github"
)

func main() {
	log.SetFlags(0)

	token := flag.String("token", "", "personal access token")
	flag.Parse()
	if flag.NArg() != 1 {
		log.Fatal("path required")
	}
	mountPath := flag.Arg(0)
	log.Printf("mounting to: %s", mountPath)

	conn, err := fuse.Mount(mountPath)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	client := newGitHubClient(*token)
	filesys := &FS{Client: client}
	if err := fs.Serve(conn, filesys); err != nil {
		log.Fatal(err)
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
		return nil, fuse.ENOENT
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
		return nil, fuse.ENOENT
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
		return nil, fuse.ENOENT
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
		return nil, fuse.ENOENT
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

