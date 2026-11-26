// Package remote is a small Remote Execution API v2 cache client.
package remote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/7shantanu7/kiln/internal/cas"
)

// batchLimit keeps batch requests under the common 4 MiB gRPC message limit.
const batchLimit = 2 << 20

// Client talks to one REAPI cache endpoint.
type Client struct {
	conn     *grpc.ClientConn
	cas      repb.ContentAddressableStorageClient
	ac       repb.ActionCacheClient
	bs       bspb.ByteStreamClient
	instance string
}

// Dial connects to addr ("host:port" or "grpc://host:port"). The connection
// is plaintext; put a TLS-terminating proxy in front for remote use.
func Dial(addr, instance string) (*Client, error) {
	addr = strings.TrimPrefix(addr, "grpc://")
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20)))
	if err != nil {
		return nil, err
	}
	return FromConn(conn, instance), nil
}

// FromConn wraps an existing connection.
func FromConn(conn *grpc.ClientConn, instance string) *Client {
	return &Client{
		conn:     conn,
		cas:      repb.NewContentAddressableStorageClient(conn),
		ac:       repb.NewActionCacheClient(conn),
		bs:       bspb.NewByteStreamClient(conn),
		instance: instance,
	}
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Digest returns the SHA-256 digest of b.
func Digest(b []byte) *repb.Digest {
	return &repb.Digest{Hash: cas.HashBytes(b), SizeBytes: int64(len(b))}
}

// DigestProto deterministically serializes m and returns its digest and bytes.
func DigestProto(m proto.Message) (*repb.Digest, []byte, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	return Digest(b), b, nil
}

// GetActionResult returns the cached result, or ok=false on a cache miss.
// It asks the server to inline stdout, stderr and the named output files;
// servers may ignore that, so read them with ResultBlobs.
func (c *Client) GetActionResult(ctx context.Context, action *repb.Digest, inlineFiles ...string) (res *repb.ActionResult, ok bool, err error) {
	res, err = c.ac.GetActionResult(ctx, &repb.GetActionResultRequest{
		InstanceName:      c.instance,
		ActionDigest:      action,
		InlineStdout:      true,
		InlineStderr:      true,
		InlineOutputFiles: inlineFiles,
		DigestFunction:    repb.DigestFunction_SHA256,
	})
	if status.Code(err) == codes.NotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return res, true, nil
}

// ResultBlobs returns the result's stdout, stderr and output file contents
// (by path), using inlined data where the server sent it and downloading the
// rest in one batch.
func (c *Client) ResultBlobs(ctx context.Context, ar *repb.ActionResult) (stdout, stderr []byte, files map[string][]byte, err error) {
	var need []*repb.Digest
	missing := func(raw []byte, d *repb.Digest) bool {
		return len(raw) == 0 && d != nil && d.SizeBytes > 0
	}
	if missing(ar.StdoutRaw, ar.StdoutDigest) {
		need = append(need, ar.StdoutDigest)
	}
	if missing(ar.StderrRaw, ar.StderrDigest) {
		need = append(need, ar.StderrDigest)
	}
	for _, f := range ar.OutputFiles {
		if missing(f.Contents, f.Digest) {
			need = append(need, f.Digest)
		}
	}
	got, err := c.Download(ctx, need)
	if err != nil {
		return nil, nil, nil, err
	}
	pick := func(raw []byte, d *repb.Digest) []byte {
		if missing(raw, d) {
			return got[d.Hash]
		}
		return raw
	}
	files = map[string][]byte{}
	for _, f := range ar.OutputFiles {
		b := pick(f.Contents, f.Digest)
		if f.Digest != nil && cas.HashBytes(b) != f.Digest.Hash {
			return nil, nil, nil, fmt.Errorf("output %s failed verification", f.Path)
		}
		files[f.Path] = b
	}
	return pick(ar.StdoutRaw, ar.StdoutDigest), pick(ar.StderrRaw, ar.StderrDigest), files, nil
}

// UpdateActionResult stores res for action. Upload its outputs first.
func (c *Client) UpdateActionResult(ctx context.Context, action *repb.Digest, res *repb.ActionResult) error {
	_, err := c.ac.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{
		InstanceName:   c.instance,
		ActionDigest:   action,
		ActionResult:   res,
		DigestFunction: repb.DigestFunction_SHA256,
	})
	return err
}

// Upload stores blobs the server does not already have.
func (c *Client) Upload(ctx context.Context, blobs [][]byte) error {
	byHash := map[string][]byte{}
	var digests []*repb.Digest
	for _, b := range blobs {
		d := Digest(b)
		if _, dup := byHash[d.Hash]; dup || d.Hash == cas.EmptyHash {
			continue
		}
		byHash[d.Hash] = b
		digests = append(digests, d)
	}
	if len(digests) == 0 {
		return nil
	}
	missing, err := c.cas.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{
		InstanceName: c.instance, BlobDigests: digests, DigestFunction: repb.DigestFunction_SHA256})
	if err != nil {
		return err
	}
	var batch []*repb.BatchUpdateBlobsRequest_Request
	var batchBytes int
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		resp, err := c.cas.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{
			InstanceName: c.instance, Requests: batch, DigestFunction: repb.DigestFunction_SHA256})
		if err != nil {
			return err
		}
		for _, r := range resp.Responses {
			if r.Status.GetCode() != 0 {
				return fmt.Errorf("upload %s: %s", r.Digest.GetHash(), r.Status.GetMessage())
			}
		}
		batch, batchBytes = nil, 0
		return nil
	}
	for _, d := range missing.MissingBlobDigests {
		data := byHash[d.Hash]
		if len(data) > batchLimit/2 {
			if err := c.write(ctx, d, data); err != nil {
				return err
			}
			continue
		}
		if batchBytes+len(data) > batchLimit {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, &repb.BatchUpdateBlobsRequest_Request{Digest: d, Data: data})
		batchBytes += len(data)
	}
	return flush()
}

func (c *Client) resource(parts ...string) string {
	if c.instance != "" {
		parts = append([]string{c.instance}, parts...)
	}
	return strings.Join(parts, "/")
}

func (c *Client) write(ctx context.Context, d *repb.Digest, data []byte) error {
	id := make([]byte, 16)
	rand.Read(id)
	name := c.resource("uploads", hex.EncodeToString(id), "blobs", d.Hash, fmt.Sprint(d.SizeBytes))
	stream, err := c.bs.Write(ctx)
	if err != nil {
		return err
	}
	const chunk = 1 << 20
	for off := 0; ; off += chunk {
		end := off + chunk
		if end > len(data) {
			end = len(data)
		}
		err := stream.Send(&bspb.WriteRequest{
			ResourceName: name, WriteOffset: int64(off), Data: data[off:end], FinishWrite: end == len(data)})
		if err == io.EOF {
			break // server already has the blob and closed the stream
		}
		if err != nil {
			return err
		}
		if end == len(data) {
			break
		}
	}
	_, err = stream.CloseAndRecv()
	return err
}

// Download fetches blobs by digest.
func (c *Client) Download(ctx context.Context, digests []*repb.Digest) (map[string][]byte, error) {
	out := map[string][]byte{}
	var batch []*repb.Digest
	var batchBytes int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		resp, err := c.cas.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{
			InstanceName: c.instance, Digests: batch, DigestFunction: repb.DigestFunction_SHA256})
		if err != nil {
			return err
		}
		for _, r := range resp.Responses {
			if r.Status.GetCode() != 0 {
				return fmt.Errorf("download %s: %s", r.Digest.GetHash(), codes.Code(r.Status.GetCode()))
			}
			if cas.HashBytes(r.Data) != r.Digest.Hash {
				return fmt.Errorf("blob %s failed verification", r.Digest.Hash)
			}
			out[r.Digest.Hash] = r.Data
		}
		batch, batchBytes = nil, 0
		return nil
	}
	for _, d := range digests {
		if d.Hash == cas.EmptyHash {
			out[d.Hash] = nil
			continue
		}
		if d.SizeBytes > batchLimit/2 {
			b, err := c.read(ctx, d)
			if err != nil {
				return nil, err
			}
			out[d.Hash] = b
			continue
		}
		if batchBytes+d.SizeBytes > batchLimit {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		batch = append(batch, d)
		batchBytes += d.SizeBytes
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) read(ctx context.Context, d *repb.Digest) ([]byte, error) {
	stream, err := c.bs.Read(ctx, &bspb.ReadRequest{ResourceName: c.resource("blobs", d.Hash, fmt.Sprint(d.SizeBytes))})
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 0, d.SizeBytes)
	for {
		r, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		buf = append(buf, r.Data...)
	}
	if int64(len(buf)) != d.SizeBytes || cas.HashBytes(buf) != d.Hash {
		return nil, fmt.Errorf("blob %s failed verification", d.Hash)
	}
	return buf, nil
}
