package cacheserver

import (
	"bytes"
	"context"
	"net"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/7shantanu7/kiln/internal/cas"
	"github.com/7shantanu7/kiln/internal/remote"
)

func newClient(t *testing.T) (*remote.Client, *Server) {
	t.Helper()
	store, err := cas.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(store)
	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer()
	srv.Register(g)
	go g.Serve(lis)
	t.Cleanup(g.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return remote.FromConn(conn, "main"), srv
}

func TestBlobRoundTripSmallAndLarge(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()
	small := []byte("small blob")
	large := bytes.Repeat([]byte("0123456789abcdef"), 1<<17) // 2 MiB, goes over ByteStream
	if err := c.Upload(ctx, [][]byte{small, large}); err != nil {
		t.Fatal(err)
	}
	got, err := c.Download(ctx, []*repb.Digest{remote.Digest(small), remote.Digest(large)})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[remote.Digest(small).Hash], small) || !bytes.Equal(got[remote.Digest(large).Hash], large) {
		t.Fatal("downloaded blobs differ from uploaded ones")
	}
}

func TestActionCache(t *testing.T) {
	c, srv := newClient(t)
	ctx := context.Background()
	action := remote.Digest([]byte("action"))
	if _, ok, err := c.GetActionResult(ctx, action); err != nil || ok {
		t.Fatalf("lookup before update: ok=%v err=%v", ok, err)
	}
	out := []byte("object file")
	res := &repb.ActionResult{OutputFiles: []*repb.OutputFile{{Path: "out.o", Digest: remote.Digest(out)}}}
	if err := c.UpdateActionResult(ctx, action, res); err == nil {
		t.Fatal("update accepted a result whose outputs were never uploaded")
	}
	if err := c.Upload(ctx, [][]byte{out}); err != nil {
		t.Fatal(err)
	}
	if err := c.UpdateActionResult(ctx, action, res); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.GetActionResult(ctx, action)
	if err != nil || !ok || got.OutputFiles[0].Digest.Hash != remote.Digest(out).Hash {
		t.Fatalf("lookup after update: %v ok=%v err=%v", got, ok, err)
	}
	if srv.Stats.ACHits.Load() != 1 || srv.Stats.ACMisses.Load() != 1 {
		t.Fatalf("hits=%d misses=%d", srv.Stats.ACHits.Load(), srv.Stats.ACMisses.Load())
	}
}

func TestParseResource(t *testing.T) {
	h := cas.EmptyHash
	for _, name := range []string{
		"blobs/" + h + "/0",
		"main/blobs/" + h + "/0",
		"main/uploads/uuid/blobs/" + h + "/0",
		"uploads/uuid/blobs/" + h + "/0/extra/metadata",
	} {
		if got, size, err := parseResource(name); err != nil || got != h || size != 0 {
			t.Errorf("parseResource(%q) = %q, %d, %v", name, got, size, err)
		}
	}
	for _, bad := range []string{"blobs/abc/1", "nothing", "compressed-blobs/zstd/" + h + "/0"} {
		if _, _, err := parseResource(bad); err == nil {
			t.Errorf("parseResource(%q) should fail", bad)
		}
	}
}
