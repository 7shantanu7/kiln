// A single bb-storage serving the CAS and Action Cache directly, small enough
// for a CI runner. Adapted from buildbarn/bb-deployments (docker-compose,
// storage.jsonnet) for the image tag pinned in .github/workflows/ci.yml.
local blocks(dir, size, newBlocks=1) = {
  'local': {
    keyLocationMap: {
      onBlockDevice: { file: { path: dir + '/key_location_map', sizeBytes: 16 * 1024 * 1024 } },
      maximumGetAttempts: 16,
      maximumPutAttempts: 64,
    },
    oldBlocks: 8,
    currentBlocks: 24,
    newBlocks: newBlocks,
    blocksOnBlockDevice: {
      source: { file: { path: dir + '/blocks', sizeBytes: size } },
      spareBlocks: 3,
    },
  },
};
{
  grpcServers: [{
    listenAddresses: [':8980'],
    authenticationPolicy: { allow: {} },
  }],
  maximumMessageSizeBytes: 16 * 1024 * 1024,
  contentAddressableStorageServer: {
    contentAddressableStorage: {
      chunkStorage: blocks('/storage/cs', 2 * 1024 * 1024 * 1024, newBlocks=3) + {
        'local'+: {
          chunkingParameters: {
            minChunkSizeBytes: 256 * 1024,
            horizonSizeBytes: 8 * 256 * 1024,
          },
        },
      },
      chunkMappingStorage: blocks('/storage/cms', 64 * 1024 * 1024),
    },
    getAuthorizer: { allow: {} },
    putAuthorizer: { allow: {} },
    findMissingAuthorizer: { allow: {} },
    maximumChunkCount: 100000,
  },
  actionCache: {
    backend: blocks('/storage/ac', 64 * 1024 * 1024),
    getAuthorizer: { allow: {} },
    putAuthorizer: { allow: {} },
  },
}
