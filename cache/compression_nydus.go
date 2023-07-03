//go:build nydus
// +build nydus

package cache

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/compression"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"

	nydusify "github.com/containerd/nydus-snapshotter/pkg/converter"
)

func init() {
	additionalAnnotations = append(
		additionalAnnotations, compression.NydusAnnotations...,
	)
}

// MergeNydus does two steps:
// 1. Extracts nydus bootstrap from nydus format (nydus blob + nydus bootstrap) for each layer.
// 2. Merge all nydus bootstraps into a final bootstrap (will as an extra layer).
// The nydus bootstrap size is very small, so the merge operation is fast.
func MergeNydus(ctx context.Context, ref ImmutableRef, comp compression.Config, s session.Group) (*ocispecs.Descriptor, []ocispecs.Descriptor, error) {
	iref, ok := ref.(*immutableRef)
	if !ok {
		return nil, nil, errors.Errorf("unsupported ref type %T", ref)
	}
	refs := iref.layerChain()
	if len(refs) == 0 {
		return nil, nil, errors.Errorf("refs can't be empty")
	}

	// Extracts nydus bootstrap from nydus format for each layer.
	var cm *cacheManager
	layers := []nydusify.Layer{}
	for _, ref := range refs {
		blobDesc, err := getBlobWithCompressionWithRetry(ctx, ref, comp, s)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "get compression blob %q", comp.Type)
		}
		ra, err := ref.cm.ContentStore.ReaderAt(ctx, blobDesc)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "get reader for compression blob %q", comp.Type)
		}
		defer ra.Close()
		if cm == nil {
			cm = ref.cm
		}
		layers = append(layers, nydusify.Layer{
			Digest:   blobDesc.Digest,
			ReaderAt: ra,
		})
	}

	// Merge all nydus bootstraps into a final nydus bootstrap.
	pr, pw := io.Pipe()
	blobDigestChan := make(chan []digest.Digest, 1)
	go func() {
		defer pw.Close()
		blobDigests, err := nydusify.Merge(ctx, layers, pw, nydusify.MergeOption{
			ChunkDictPath: comp.NydusChunkDictPath,
			WithTar:       true,
		})
		if err != nil {
			pw.CloseWithError(errors.Wrapf(err, "merge nydus bootstrap"))
		}
		blobDigestChan <- blobDigests
	}()

	// Compress final nydus bootstrap to tar.gz and write into content store.
	cw, err := content.OpenWriter(ctx, cm.ContentStore, content.WithRef("nydus-merge-"+iref.getChainID().String()))
	if err != nil {
		return nil, nil, errors.Wrap(err, "open content store writer")
	}
	defer cw.Close()

	gw := gzip.NewWriter(cw)
	uncompressedDgst := digest.SHA256.Digester()
	compressed := io.MultiWriter(gw, uncompressedDgst.Hash())
	if _, err := io.Copy(compressed, pr); err != nil {
		return nil, nil, errors.Wrapf(err, "copy bootstrap targz into content store")
	}
	if err := gw.Close(); err != nil {
		return nil, nil, errors.Wrap(err, "close gzip writer")
	}

	compressedDgst := cw.Digest()
	if err := cw.Commit(ctx, 0, compressedDgst, content.WithLabels(map[string]string{
		containerdUncompressed: uncompressedDgst.Digest().String(),
	})); err != nil {
		if !errdefs.IsAlreadyExists(err) {
			return nil, nil, errors.Wrap(err, "commit to content store")
		}
	}
	if err := cw.Close(); err != nil {
		return nil, nil, errors.Wrap(err, "close content store writer")
	}

	bootstrapInfo, err := cm.ContentStore.Info(ctx, compressedDgst)
	if err != nil {
		return nil, nil, errors.Wrap(err, "get info from content store")
	}

	blobDigests := <-blobDigestChan
	blobDescs := []ocispecs.Descriptor{}
	blobIDs := []string{}
	uniqBlobDigests := map[digest.Digest]bool{}
	for _, blobDigest := range blobDigests {
		if uniqBlobDigests[blobDigest] {
			continue
		}
		uniqBlobDigests[blobDigest] = true
		blobInfo, err := cm.ContentStore.Info(ctx, blobDigest)
		if err != nil {
			return nil, nil, errors.Wrap(err, "get info from content store")
		}
		blobDesc := ocispecs.Descriptor{
			Digest:    blobDigest,
			Size:      blobInfo.Size,
			MediaType: nydusify.MediaTypeNydusBlob,
			Annotations: map[string]string{
				nydusify.LayerAnnotationNydusBlob: "true",
				containerdUncompressed:            blobDigest.String(),
			},
		}
		blobDescs = append(blobDescs, blobDesc)
		blobIDs = append(blobIDs, blobDigest.Hex())
	}

	blobIDsBytes, err := json.Marshal(blobIDs)
	if err != nil {
		return nil, nil, errors.Wrap(err, "marshal blob ids")
	}

	desc := ocispecs.Descriptor{
		Digest:    compressedDgst,
		Size:      bootstrapInfo.Size,
		MediaType: ocispecs.MediaTypeImageLayerGzip,
		Annotations: map[string]string{
			containerdUncompressed:            uncompressedDgst.Digest().String(),
			nydusify.LayerAnnotationFSVersion: comp.NydusFsVersion,
			// Use this annotation to identify nydus bootstrap layer.
			nydusify.LayerAnnotationNydusBootstrap: "true",
			// Track all blob digests for nydus snapshotter.
			nydusify.LayerAnnotationNydusBlobIDs: string(blobIDsBytes),
		},
	}

	return &desc, blobDescs, nil
}
