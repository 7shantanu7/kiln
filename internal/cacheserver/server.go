// Package cacheserver implements the cache half of the Bazel Remote Execution
// API v2 (ContentAddressableStorage, ActionCache, Capabilities) plus the
// ByteStream service for large blobs. It does not implement remote execution.
package cacheserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	smpb "github.com/bazelbuild/remote-apis/build/bazel/semver"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/7shantanu7/kiln/internal/cas"
)

// MaxBatchBytes is the largest total payload accepted by the batch calls.
const MaxBatchBytes = 4 << 20

// Stats are cumulative counters exported on /metrics.
type Stats struct {
	ACHits, ACMisses, ACUpdates     atomic.Int64
	BlobsWritten, BytesWritten      atomic.Int64
	BlobsRead, BytesRead            atomic.Int64
	FindMissingQueried, FindMissing atomic.Int64
}

// Server holds the store and implements all four gRPC services.
type Server struct {
	repb.UnimplementedContentAddressableStorageServer
	repb.UnimplementedActionCacheServer
	repb.UnimplementedCapabilitiesServer
	bspb.UnimplementedByteStreamServer

	Store *cas.Store
	Stats Stats

	writesMu sync.Mutex
	writes   map[string]int64 // committed upload resource name -> size
}

// New returns a server backed by store.
func New(store *cas.Store) *Server {
	return &Server{Store: store, writes: map[string]int64{}}
}

// Register adds the services to a gRPC server.
func (s *Server) Register(g *grpc.Server) {
	repb.RegisterContentAddressableStorageServer(g, s)
	repb.RegisterActionCacheServer(g, s)
	repb.RegisterCapabilitiesServer(g, s)
	bspb.RegisterByteStreamServer(g, s)
}

func checkDigest(d *repb.Digest) error {
	if d == nil {
		return grpcstatus.Error(codes.InvalidArgument, "missing digest")
	}
	if len(d.Hash) != 64 || d.SizeBytes < 0 {
		return grpcstatus.Errorf(codes.InvalidArgument, "invalid digest %s/%d", d.Hash, d.SizeBytes)
	}
	return nil
}

func checkDigestFunction(f repb.DigestFunction_Value) error {
	if f != repb.DigestFunction_UNKNOWN && f != repb.DigestFunction_SHA256 {
		return grpcstatus.Errorf(codes.InvalidArgument, "only SHA256 is supported, got %v", f)
	}
	return nil
}

// GetCapabilities advertises a SHA-256 cache with action cache updates allowed.
func (s *Server) GetCapabilities(ctx context.Context, req *repb.GetCapabilitiesRequest) (*repb.ServerCapabilities, error) {
	return &repb.ServerCapabilities{
		CacheCapabilities: &repb.CacheCapabilities{
			DigestFunctions:                 []repb.DigestFunction_Value{repb.DigestFunction_SHA256},
			ActionCacheUpdateCapabilities:   &repb.ActionCacheUpdateCapabilities{UpdateEnabled: true},
			MaxBatchTotalSizeBytes:          MaxBatchBytes,
			SymlinkAbsolutePathStrategy:     repb.SymlinkAbsolutePathStrategy_DISALLOWED,
			SupportedCompressors:            nil,
			SupportedBatchUpdateCompressors: nil,
		},
		LowApiVersion:  &smpb.SemVer{Major: 2},
		HighApiVersion: &smpb.SemVer{Major: 2, Minor: 3},
	}, nil
}

// FindMissingBlobs returns the digests not present in the CAS.
func (s *Server) FindMissingBlobs(ctx context.Context, req *repb.FindMissingBlobsRequest) (*repb.FindMissingBlobsResponse, error) {
	if err := checkDigestFunction(req.DigestFunction); err != nil {
		return nil, err
	}
	resp := &repb.FindMissingBlobsResponse{}
	for _, d := range req.BlobDigests {
		if err := checkDigest(d); err != nil {
			return nil, err
		}
		s.Stats.FindMissingQueried.Add(1)
		if !s.Store.Has(cas.CAS, d.Hash) {
			resp.MissingBlobDigests = append(resp.MissingBlobDigests, d)
			s.Stats.FindMissing.Add(1)
		}
	}
	return resp, nil
}

// BatchUpdateBlobs stores small blobs, verifying each digest.
func (s *Server) BatchUpdateBlobs(ctx context.Context, req *repb.BatchUpdateBlobsRequest) (*repb.BatchUpdateBlobsResponse, error) {
	if err := checkDigestFunction(req.DigestFunction); err != nil {
		return nil, err
	}
	var total int64
	for _, r := range req.Requests {
		total += int64(len(r.Data))
	}
	if total > MaxBatchBytes {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "batch of %d bytes exceeds %d", total, MaxBatchBytes)
	}
	resp := &repb.BatchUpdateBlobsResponse{}
	for _, r := range req.Requests {
		st := &status.Status{}
		if err := checkDigest(r.Digest); err != nil {
			st.Code, st.Message = int32(codes.InvalidArgument), err.Error()
		} else if r.Compressor != repb.Compressor_IDENTITY {
			st.Code, st.Message = int32(codes.InvalidArgument), "compression not supported"
		} else if int64(len(r.Data)) != r.Digest.SizeBytes {
			st.Code, st.Message = int32(codes.InvalidArgument), "size does not match digest"
		} else if err := s.Store.Put(cas.CAS, r.Digest.Hash, r.Data); err != nil {
			st.Code, st.Message = int32(codes.InvalidArgument), err.Error()
		} else {
			s.Stats.BlobsWritten.Add(1)
			s.Stats.BytesWritten.Add(int64(len(r.Data)))
		}
		resp.Responses = append(resp.Responses, &repb.BatchUpdateBlobsResponse_Response{Digest: r.Digest, Status: st})
	}
	return resp, nil
}

// BatchReadBlobs returns small blobs.
func (s *Server) BatchReadBlobs(ctx context.Context, req *repb.BatchReadBlobsRequest) (*repb.BatchReadBlobsResponse, error) {
	if err := checkDigestFunction(req.DigestFunction); err != nil {
		return nil, err
	}
	var total int64
	for _, d := range req.Digests {
		if err := checkDigest(d); err != nil {
			return nil, err
		}
		total += d.SizeBytes
	}
	if total > MaxBatchBytes {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "batch of %d bytes exceeds %d", total, MaxBatchBytes)
	}
	resp := &repb.BatchReadBlobsResponse{}
	for _, d := range req.Digests {
		r := &repb.BatchReadBlobsResponse_Response{Digest: d, Status: &status.Status{}}
		data, err := s.Store.Get(cas.CAS, d.Hash)
		switch {
		case errors.Is(err, cas.ErrNotFound):
			r.Status.Code = int32(codes.NotFound)
		case err != nil:
			r.Status.Code, r.Status.Message = int32(codes.Internal), err.Error()
		default:
			r.Data = data
			s.Stats.BlobsRead.Add(1)
			s.Stats.BytesRead.Add(int64(len(data)))
		}
		resp.Responses = append(resp.Responses, r)
	}
	return resp, nil
}

// GetTree returns every Directory under the root digest in one page.
func (s *Server) GetTree(req *repb.GetTreeRequest, stream repb.ContentAddressableStorage_GetTreeServer) error {
	if err := checkDigest(req.RootDigest); err != nil {
		return err
	}
	var dirs []*repb.Directory
	queue := []*repb.Digest{req.RootDigest}
	seen := map[string]bool{}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		if seen[d.Hash] {
			continue
		}
		seen[d.Hash] = true
		data, err := s.Store.Get(cas.CAS, d.Hash)
		if errors.Is(err, cas.ErrNotFound) {
			return grpcstatus.Errorf(codes.NotFound, "directory %s not found", d.Hash)
		}
		if err != nil {
			return err
		}
		dir := &repb.Directory{}
		if err := proto.Unmarshal(data, dir); err != nil {
			return grpcstatus.Errorf(codes.InvalidArgument, "blob %s is not a Directory: %v", d.Hash, err)
		}
		dirs = append(dirs, dir)
		for _, sub := range dir.Directories {
			queue = append(queue, sub.Digest)
		}
	}
	return stream.Send(&repb.GetTreeResponse{Directories: dirs})
}

// GetActionResult looks up an action. Results whose outputs have been evicted
// from the CAS are reported as missing so the client rebuilds them.
func (s *Server) GetActionResult(ctx context.Context, req *repb.GetActionResultRequest) (*repb.ActionResult, error) {
	if err := checkDigest(req.ActionDigest); err != nil {
		return nil, err
	}
	data, err := s.Store.Get(cas.AC, req.ActionDigest.Hash)
	if errors.Is(err, cas.ErrNotFound) {
		s.Stats.ACMisses.Add(1)
		return nil, grpcstatus.Error(codes.NotFound, "action not in cache")
	}
	if err != nil {
		return nil, err
	}
	res := &repb.ActionResult{}
	if err := proto.Unmarshal(data, res); err != nil {
		return nil, grpcstatus.Errorf(codes.Internal, "corrupt action result: %v", err)
	}
	for _, f := range res.OutputFiles {
		if !s.Store.Has(cas.CAS, f.Digest.GetHash()) {
			s.Stats.ACMisses.Add(1)
			return nil, grpcstatus.Error(codes.NotFound, "action outputs evicted")
		}
	}
	for _, d := range []*repb.Digest{res.StdoutDigest, res.StderrDigest} {
		if d != nil && d.SizeBytes > 0 && !s.Store.Has(cas.CAS, d.Hash) {
			s.Stats.ACMisses.Add(1)
			return nil, grpcstatus.Error(codes.NotFound, "action logs evicted")
		}
	}
	s.Stats.ACHits.Add(1)
	return res, nil
}

// UpdateActionResult stores a result. Output blobs must already be uploaded.
func (s *Server) UpdateActionResult(ctx context.Context, req *repb.UpdateActionResultRequest) (*repb.ActionResult, error) {
	if err := checkDigest(req.ActionDigest); err != nil {
		return nil, err
	}
	if req.ActionResult == nil {
		return nil, grpcstatus.Error(codes.InvalidArgument, "missing action result")
	}
	for _, f := range req.ActionResult.OutputFiles {
		if err := checkDigest(f.Digest); err != nil {
			return nil, err
		}
		if !s.Store.Has(cas.CAS, f.Digest.Hash) {
			return nil, grpcstatus.Errorf(codes.FailedPrecondition, "output %s (%s) not uploaded", f.Path, f.Digest.Hash)
		}
	}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(req.ActionResult)
	if err != nil {
		return nil, err
	}
	if err := s.Store.Put(cas.AC, req.ActionDigest.Hash, data); err != nil {
		return nil, err
	}
	s.Stats.ACUpdates.Add(1)
	return req.ActionResult, nil
}

// parseResource extracts hash and size from a ByteStream resource name:
// "[instance/]blobs/<hash>/<size>" or "[instance/]uploads/<uuid>/blobs/<hash>/<size>[/...]".
func parseResource(name string) (hash string, size int64, err error) {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		if p == "blobs" && i+2 < len(parts) {
			if parts[i+1] == "compressed-blobs" {
				break
			}
			size, err = strconv.ParseInt(parts[i+2], 10, 64)
			if err != nil || len(parts[i+1]) != 64 {
				break
			}
			return parts[i+1], size, nil
		}
		if p == "compressed-blobs" {
			return "", 0, grpcstatus.Error(codes.InvalidArgument, "compressed blobs are not supported")
		}
	}
	return "", 0, grpcstatus.Errorf(codes.InvalidArgument, "bad resource name %q", name)
}

// Read streams a blob.
func (s *Server) Read(req *bspb.ReadRequest, stream bspb.ByteStream_ReadServer) error {
	hash, size, err := parseResource(req.ResourceName)
	if err != nil {
		return err
	}
	if hash == cas.EmptyHash {
		return nil
	}
	f, n, err := s.Store.OpenBlob(hash)
	if errors.Is(err, cas.ErrNotFound) {
		return grpcstatus.Errorf(codes.NotFound, "blob %s not found", hash)
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if n != size {
		return grpcstatus.Errorf(codes.InvalidArgument, "blob %s has %d bytes, request says %d", hash, n, size)
	}
	if req.ReadOffset < 0 || req.ReadOffset > n {
		return grpcstatus.Error(codes.OutOfRange, "read offset out of range")
	}
	if _, err := f.Seek(req.ReadOffset, io.SeekStart); err != nil {
		return err
	}
	var r io.Reader = f
	if req.ReadLimit > 0 {
		r = io.LimitReader(f, req.ReadLimit)
	}
	buf := make([]byte, 1<<20)
	for {
		k, err := r.Read(buf)
		if k > 0 {
			if err := stream.Send(&bspb.ReadResponse{Data: buf[:k]}); err != nil {
				return err
			}
			s.Stats.BytesRead.Add(int64(k))
		}
		if err == io.EOF {
			s.Stats.BlobsRead.Add(1)
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Write receives a blob upload, verifying its digest before committing.
func (s *Server) Write(stream bspb.ByteStream_WriteServer) error {
	var (
		w        *cas.Writer
		resource string
		size     int64
		offset   int64
	)
	defer func() {
		if w != nil {
			w.Abort()
		}
	}()
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return grpcstatus.Error(codes.InvalidArgument, "upload ended without finish_write")
		}
		if err != nil {
			return err
		}
		if w == nil {
			resource = req.ResourceName
			hash, sz, err := parseResource(resource)
			if err != nil {
				return err
			}
			size = sz
			if s.Store.Has(cas.CAS, hash) {
				// Already present: tell the client it can stop sending.
				return stream.SendAndClose(&bspb.WriteResponse{CommittedSize: size})
			}
			if w, err = s.Store.NewWriter(cas.CAS, hash); err != nil {
				return err
			}
		}
		if req.WriteOffset != offset {
			return grpcstatus.Errorf(codes.InvalidArgument, "write offset %d, expected %d", req.WriteOffset, offset)
		}
		if _, err := w.Write(req.Data); err != nil {
			return err
		}
		offset += int64(len(req.Data))
		if req.FinishWrite {
			err := w.Commit(size)
			w = nil
			if err != nil {
				return grpcstatus.Error(codes.InvalidArgument, err.Error())
			}
			s.writesMu.Lock()
			s.writes[resource] = size
			s.writesMu.Unlock()
			s.Stats.BlobsWritten.Add(1)
			s.Stats.BytesWritten.Add(size)
			return stream.SendAndClose(&bspb.WriteResponse{CommittedSize: size})
		}
	}
}

// QueryWriteStatus reports whether an upload finished.
func (s *Server) QueryWriteStatus(ctx context.Context, req *bspb.QueryWriteStatusRequest) (*bspb.QueryWriteStatusResponse, error) {
	hash, size, err := parseResource(req.ResourceName)
	if err != nil {
		return nil, err
	}
	if s.Store.Has(cas.CAS, hash) {
		return &bspb.QueryWriteStatusResponse{CommittedSize: size, Complete: true}, nil
	}
	return &bspb.QueryWriteStatusResponse{}, nil
}

// Metrics renders the counters in Prometheus text format.
func (s *Server) Metrics() string {
	entries, bytes, evicted := s.Store.Usage()
	var b strings.Builder
	line := func(name, help string, v int64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	line("kiln_cache_ac_hits_total", "Action cache hits.", s.Stats.ACHits.Load())
	line("kiln_cache_ac_misses_total", "Action cache misses.", s.Stats.ACMisses.Load())
	line("kiln_cache_ac_updates_total", "Action results stored.", s.Stats.ACUpdates.Load())
	line("kiln_cache_blobs_written_total", "Blobs written to the CAS.", s.Stats.BlobsWritten.Load())
	line("kiln_cache_bytes_written_total", "Bytes written to the CAS.", s.Stats.BytesWritten.Load())
	line("kiln_cache_blobs_read_total", "Blobs read from the CAS.", s.Stats.BlobsRead.Load())
	line("kiln_cache_bytes_read_total", "Bytes read from the CAS.", s.Stats.BytesRead.Load())
	line("kiln_cache_evictions_total", "Entries evicted to stay under the size limit.", evicted)
	fmt.Fprintf(&b, "# HELP kiln_cache_entries Entries on disk.\n# TYPE kiln_cache_entries gauge\nkiln_cache_entries %d\n", entries)
	fmt.Fprintf(&b, "# HELP kiln_cache_bytes Bytes on disk.\n# TYPE kiln_cache_bytes gauge\nkiln_cache_bytes %d\n", bytes)
	return b.String()
}
