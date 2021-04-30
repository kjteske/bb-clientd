package fuse

import (
	"context"
	"sync"
	"syscall"

	"github.com/buildbarn/bb-clientd/pkg/cas"
	re_fuse "github.com/buildbarn/bb-remote-execution/pkg/filesystem/fuse"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/remoteoutputservice"
	"github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/buildbarn/bb-storage/pkg/random"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/hanwen/go-fuse/v2/fuse"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type buildState struct {
	id                 string
	digestFunction     digest.Function
	scopeWalkerFactory *path.VirtualRootScopeWalkerFactory
}

type outputPathState struct {
	buildState               *buildState
	rootDirectory            OutputPath
	rootDirectoryInodeNumber uint64
	casFileFactory           re_fuse.CASFileFactory
}

// RemoteOutputServiceDirectory is FUSE directory that acts as the
// top-level directory for Remote Output Service. The Remote Output
// Service can be used by build clients to efficiently populate a
// directory with build outputs.
//
// In addition to acting as a FUSE directory, this type also implements
// a gRPC server for the Remote Output Service. This gRPC service can be
// used to start and finalize builds, but also to perform bulk creation
// and stat() operations.
//
// This implementation of the Remote Output Service is relatively
// simple:
//
// - There is no persistency of build information across restarts.
// - No snapshotting of completed builds takes place, meaning that only
//   the results of the latest build of a given output base are exposed.
// - Every output path is backed by an InMemoryPrepopulatedDirectory,
//   meaning that memory usage may be high.
// - No automatic garbage collection of old output paths is performed.
//
// This implementation should eventually be extended to address the
// issues listed above.
type RemoteOutputServiceDirectory struct {
	readOnlyDirectory

	inodeNumber               uint64
	inodeNumberGenerator      random.SingleThreadedGenerator
	entryNotifier             re_fuse.EntryNotifier
	outputPathFactory         OutputPathFactory
	contentAddressableStorage blobstore.BlobAccess
	indexedTreeFetcher        cas.IndexedTreeFetcher

	lock          sync.Mutex
	outputBaseIDs map[path.Component]*outputPathState
	buildIDs      map[string]*outputPathState
}

var (
	_ re_fuse.Directory                             = &RemoteOutputServiceDirectory{}
	_ remoteoutputservice.RemoteOutputServiceServer = &RemoteOutputServiceDirectory{}
)

// NewRemoteOutputServiceDirectory creates a new instance of
// RemoteOutputServiceDirectory.
func NewRemoteOutputServiceDirectory(inodeNumber uint64, inodeNumberGenerator random.SingleThreadedGenerator, entryNotifier re_fuse.EntryNotifier, outputPathFactory OutputPathFactory, contentAddressableStorage blobstore.BlobAccess, indexedTreeFetcher cas.IndexedTreeFetcher) *RemoteOutputServiceDirectory {
	return &RemoteOutputServiceDirectory{
		inodeNumber:               inodeNumber,
		inodeNumberGenerator:      inodeNumberGenerator,
		entryNotifier:             entryNotifier,
		outputPathFactory:         outputPathFactory,
		contentAddressableStorage: contentAddressableStorage,
		indexedTreeFetcher:        indexedTreeFetcher,

		outputBaseIDs: map[path.Component]*outputPathState{},
		buildIDs:      map[string]*outputPathState{},
	}
}

// Clean all build outputs associated with a single output base.
func (d *RemoteOutputServiceDirectory) Clean(ctx context.Context, request *remoteoutputservice.CleanRequest) (*emptypb.Empty, error) {
	outputBaseID, ok := path.NewComponent(request.OutputBaseId)
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "Output base ID is not a valid filename")
	}

	d.lock.Lock()
	outputPathState, ok := d.outputBaseIDs[outputBaseID]
	d.lock.Unlock()
	if ok {
		// Remove all data stored inside the output path. This
		// must be done without holding the directory lock, as
		// EntryNotifier calls generated by the output path
		// could deadlock otherwise.
		if err := outputPathState.rootDirectory.RemoveAllChildren(true); err != nil {
			return nil, err
		}

		d.lock.Lock()
		if outputPathState == d.outputBaseIDs[outputBaseID] {
			delete(d.outputBaseIDs, outputBaseID)
			if buildState := outputPathState.buildState; buildState != nil {
				delete(d.buildIDs, buildState.id)
				outputPathState.buildState = nil
			}
		}
		d.lock.Unlock()

		d.entryNotifier(d.inodeNumber, outputBaseID)
	} else if err := d.outputPathFactory.Clean(outputBaseID); err != nil {
		// This output path hasn't been accessed since startup.
		// It may be the case that there is persistent state
		// associated with this output path, so make sure that
		// is removed as well.
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// findMissingAndRemove is called during StartBuild() to remove a single
// batch of files from the output path that are no longer present in the
// Content Addressable Storage.
func (d *RemoteOutputServiceDirectory) findMissingAndRemove(ctx context.Context, queue map[digest.Digest][]func() error) error {
	set := digest.NewSetBuilder()
	for digest := range queue {
		set.Add(digest)
	}
	missing, err := d.contentAddressableStorage.FindMissing(ctx, set.Build())
	if err != nil {
		return util.StatusWrap(err, "Failed to find missing blobs")
	}
	for _, digest := range missing.Items() {
		for _, removeFunc := range queue[digest] {
			if err := removeFunc(); err != nil {
				return util.StatusWrapf(err, "Failed to remove file with digest %#v", digest.String())
			}
		}
	}
	return nil
}

// filterMissingChildren is called during StartBuild() to traverse over
// all files in the output path, calling FindMissingBlobs() on them to
// ensure that they will not disappear during the build. Any files that
// are missing are removed from the output path.
func (d *RemoteOutputServiceDirectory) filterMissingChildren(ctx context.Context, rootDirectory re_fuse.PrepopulatedDirectory, digestFunction digest.Function) error {
	queue := map[digest.Digest][]func() error{}
	var savedErr error
	if err := rootDirectory.FilterChildren(func(node re_fuse.InitialNode, removeFunc re_fuse.ChildRemover) bool {
		// Obtain the transitive closure of digests on which
		// this file or directory depends.
		var digests digest.Set
		if node.Leaf != nil {
			digests = node.Leaf.GetContainingDigests()
		} else if digests, savedErr = node.Directory.GetContainingDigests(ctx); savedErr != nil {
			// Can't compute the set of digests underneath
			// this directory. Remove the directory
			// entirely.
			if status.Code(savedErr) == codes.NotFound {
				savedErr = nil
				if err := removeFunc(); err != nil {
					savedErr = util.StatusWrap(err, "Failed to remove non-existent directory")
					return false
				}
				return true
			}
			return false
		}

		// Remove files that use a different instance name or
		// digest function. It may be technically valid to
		// retain these, but it comes at the cost of requiring
		// the build client to copy files between clusters, or
		// reupload them with a different hash. This may be
		// slower than requiring a rebuild.
		for _, blobDigest := range digests.Items() {
			if !blobDigest.UsesDigestFunction(digestFunction) {
				if err := removeFunc(); err != nil {
					savedErr = util.StatusWrapf(err, "Failed to remove file with different instance name or digest function with digest %#v", blobDigest.String())
					return false
				}
				return true
			}
		}

		for _, blobDigest := range digests.Items() {
			if len(queue) >= blobstore.RecommendedFindMissingDigestsCount {
				// Maximum number of digests reached.
				savedErr = d.findMissingAndRemove(ctx, queue)
				if savedErr != nil {
					return false
				}
				queue = map[digest.Digest][]func() error{}
			}
			queue[blobDigest] = append(queue[blobDigest], removeFunc)
		}
		return true
	}); err != nil {
		return err
	}
	if savedErr != nil {
		return savedErr
	}

	// Process the final batch of files.
	if len(queue) > 0 {
		return d.findMissingAndRemove(ctx, queue)
	}
	return nil
}

// StartBuild is called by a build client to indicate that a new build
// in a given output base is starting.
func (d *RemoteOutputServiceDirectory) StartBuild(ctx context.Context, request *remoteoutputservice.StartBuildRequest) (*remoteoutputservice.StartBuildResponse, error) {
	// Compute the full output path and the output path suffix. The
	// former needs to be used by us, while the latter is
	// communicated back to the client.
	outputPath, scopeWalker := path.EmptyBuilder.Join(path.NewAbsoluteScopeWalker(path.VoidComponentWalker))
	if err := path.Resolve(request.OutputPathPrefix, scopeWalker); err != nil {
		return nil, util.StatusWrap(err, "Failed to resolve output path prefix")
	}
	outputPathSuffix, scopeWalker := path.EmptyBuilder.Join(path.VoidScopeWalker)
	outputPath, scopeWalker = outputPath.Join(scopeWalker)
	outputBaseID, ok := path.NewComponent(request.OutputBaseId)
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "Output base ID is not a valid filename")
	}
	componentWalker, err := scopeWalker.OnScope(false)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to resolve output path")
	}
	if _, err := componentWalker.OnTerminal(outputBaseID); err != nil {
		return nil, util.StatusWrap(err, "Failed to resolve output path")
	}

	// Create a virtual root based on the output path and provided
	// aliases. This will be used to properly resolve targets of
	// symbolic links stored in the output path.
	scopeWalkerFactory, err := path.NewVirtualRootScopeWalkerFactory(outputPath.String(), request.OutputPathAliases)
	if err != nil {
		return nil, err
	}

	instanceName, err := digest.NewInstanceName(request.InstanceName)
	if err != nil {
		return nil, util.StatusWrapf(err, "Failed to parse instance name %#v", request.InstanceName)
	}
	digestFunction, err := instanceName.GetDigestFunction(request.DigestFunction)
	if err != nil {
		return nil, err
	}

	d.lock.Lock()
	state, ok := d.buildIDs[request.BuildId]
	if !ok {
		state, ok = d.outputBaseIDs[outputBaseID]
		if ok {
			if buildState := state.buildState; buildState != nil {
				// A previous build is running that wasn't
				// finalized properly. Forcefully finalize it.
				delete(d.buildIDs, buildState.id)
				state.buildState = nil
			}
		} else {
			// No previous builds have been run for this
			// output base. Create a new output path.
			//
			// TODO: This should not use DefaultErrorLogger.
			// Instead, we should capture errors, so that we
			// can propagate them back to the build client.
			// This allows the client to retry, or at least
			// display the error immediately, so that users
			// don't need to check logs.
			errorLogger := util.DefaultErrorLogger
			inodeNumber := d.inodeNumberGenerator.Uint64()
			casFileFactory := re_fuse.NewBlobAccessCASFileFactory(
				context.Background(),
				d.contentAddressableStorage,
				errorLogger)
			state = &outputPathState{
				rootDirectory:            d.outputPathFactory.StartInitialBuild(outputBaseID, casFileFactory, instanceName, errorLogger, inodeNumber),
				rootDirectoryInodeNumber: inodeNumber,
				casFileFactory:           casFileFactory,
			}
			d.outputBaseIDs[outputBaseID] = state
		}

		// Allow BatchCreate() and BatchStat() requests for the
		// new build ID.
		state.buildState = &buildState{
			id:                 request.BuildId,
			digestFunction:     digestFunction,
			scopeWalkerFactory: scopeWalkerFactory,
		}
		d.buildIDs[request.BuildId] = state
	}
	d.lock.Unlock()

	// Call ContentAddressableStorage.FindMissingBlobs() on all of
	// the files and tree objects contained within the output path,
	// so that we have the certainty that they don't disappear
	// during the build. Remove all of the files and directories
	// that are missing, so that the client can detect their absence
	// and rebuild them.
	if err := d.filterMissingChildren(ctx, state.rootDirectory, digestFunction); err != nil {
		return nil, util.StatusWrap(err, "Failed to filter contents of the output path")
	}

	return &remoteoutputservice.StartBuildResponse{
		// TODO: Fill in InitialOutputPathContents, so that the
		// client can skip parts of its analysis. The easiest
		// way to achieve this would be to freeze the contents
		// of the output path between builds.
		OutputPathSuffix: outputPathSuffix.String(),
	}, nil
}

// getOutputPathAndBuildState returns the state objects associated with
// a given build ID. This function is used by all gRPC methods that can
// only be invoked as part of a build (e.g., BatchCreate(), BatchStat()).
func (d *RemoteOutputServiceDirectory) getOutputPathAndBuildState(buildID string) (*outputPathState, *buildState, error) {
	d.lock.Lock()
	defer d.lock.Unlock()

	outputPathState, ok := d.buildIDs[buildID]
	if !ok {
		return nil, nil, status.Error(codes.FailedPrecondition, "Build ID is not associated with any running build")
	}
	return outputPathState, outputPathState.buildState, nil
}

// directoryCreatingComponentWalker is an implementation of
// ComponentWalker that is used by BatchCreate() to resolve the path
// prefix under which all provided files, symbolic links and directories
// should be created.
//
// This resolver forcefully creates all intermediate pathname
// components, removing any non-directories that are in the way.
type directoryCreatingComponentWalker struct {
	stack []re_fuse.PrepopulatedDirectory
}

func (cw *directoryCreatingComponentWalker) OnDirectory(name path.Component) (path.GotDirectoryOrSymlink, error) {
	d := cw.stack[len(cw.stack)-1]
	child, err := d.CreateAndEnterPrepopulatedDirectory(name)
	if err != nil {
		return nil, err
	}
	cw.stack = append(cw.stack, child)
	return path.GotDirectory{
		Child:        cw,
		IsReversible: true,
	}, nil
}

func (cw *directoryCreatingComponentWalker) OnTerminal(name path.Component) (*path.GotSymlink, error) {
	return path.OnTerminalViaOnDirectory(cw, name)
}

func (cw *directoryCreatingComponentWalker) OnUp() (path.ComponentWalker, error) {
	if len(cw.stack) == 1 {
		return nil, status.Error(codes.InvalidArgument, "Path resolves to a location outside the output path")
	}
	cw.stack = cw.stack[:len(cw.stack)-1]
	return cw, nil
}

func (cw *directoryCreatingComponentWalker) createChild(outputPath string, initialNode re_fuse.InitialNode) error {
	outputParentCreator := parentDirectoryCreatingComponentWalker{
		stack: append([]re_fuse.PrepopulatedDirectory(nil), cw.stack...),
	}
	if err := path.Resolve(outputPath, path.NewRelativeScopeWalker(&outputParentCreator)); err != nil {
		return util.StatusWrap(err, "Failed to resolve path")
	}
	name := outputParentCreator.name
	if name == nil {
		return status.Errorf(codes.InvalidArgument, "Path resolves to a directory")
	}
	return outputParentCreator.stack[len(outputParentCreator.stack)-1].CreateChildren(
		map[path.Component]re_fuse.InitialNode{
			*name: initialNode,
		},
		true)
}

// parentDirectoryCreatingComponentWalker is an implementation of
// ComponentWalker that is used by BatchCreate() to resolve the parent
// directory of the path where a file, directory or symlink needs to be
// created.
type parentDirectoryCreatingComponentWalker struct {
	stack []re_fuse.PrepopulatedDirectory
	name  *path.Component
}

func (cw *parentDirectoryCreatingComponentWalker) OnDirectory(name path.Component) (path.GotDirectoryOrSymlink, error) {
	d := cw.stack[len(cw.stack)-1]
	child, err := d.CreateAndEnterPrepopulatedDirectory(name)
	if err != nil {
		return nil, err
	}
	cw.stack = append(cw.stack, child)
	return path.GotDirectory{
		Child:        cw,
		IsReversible: true,
	}, nil
}

func (cw *parentDirectoryCreatingComponentWalker) OnTerminal(name path.Component) (*path.GotSymlink, error) {
	cw.name = &name
	return nil, nil
}

func (cw *parentDirectoryCreatingComponentWalker) OnUp() (path.ComponentWalker, error) {
	if len(cw.stack) == 1 {
		return nil, status.Error(codes.InvalidArgument, "Path resolves to a location outside the output path")
	}
	cw.stack = cw.stack[:len(cw.stack)-1]
	return cw, nil
}

// BatchCreate can be called by a build client to create files, symbolic
// links and directories.
//
// Because files and directories are provided in the form of OutputFile
// and OutputDirectory messages, this implementation is capable of
// creating files and directories whose contents get loaded from the
// Content Addressable Storage lazily.
func (d *RemoteOutputServiceDirectory) BatchCreate(ctx context.Context, request *remoteoutputservice.BatchCreateRequest) (*emptypb.Empty, error) {
	outputPathState, buildState, err := d.getOutputPathAndBuildState(request.BuildId)
	if err != nil {
		return nil, err
	}

	// Resolve the path prefix. Optionally, remove all of its contents.
	prefixCreator := directoryCreatingComponentWalker{
		stack: []re_fuse.PrepopulatedDirectory{outputPathState.rootDirectory},
	}
	if err := path.Resolve(request.PathPrefix, path.NewRelativeScopeWalker(&prefixCreator)); err != nil {
		return nil, util.StatusWrap(err, "Failed to create path prefix directory")
	}
	if request.CleanPathPrefix {
		if err := prefixCreator.stack[len(prefixCreator.stack)-1].RemoveAllChildren(false); err != nil {
			return nil, util.StatusWrap(err, "Failed to clean path prefix directory")
		}
	}

	// Create requested files.
	instanceName := buildState.digestFunction.GetInstanceName()
	for _, entry := range request.Files {
		childDigest, err := instanceName.NewDigestFromProto(entry.Digest)
		if err != nil {
			return nil, util.StatusWrapf(err, "Invalid digest for file %#v", entry.Path)
		}
		var out fuse.Attr
		if err := prefixCreator.createChild(entry.Path, re_fuse.InitialNode{
			Leaf: outputPathState.casFileFactory.LookupFile(childDigest, entry.IsExecutable, &out),
		}); err != nil {
			return nil, util.StatusWrapf(err, "Failed to create file %#v", entry.Path)
		}
	}

	// Create requested directories.
	for _, entry := range request.Directories {
		childDigest, err := instanceName.NewDigestFromProto(entry.TreeDigest)
		if err != nil {
			return nil, util.StatusWrapf(err, "Invalid digest for directory %#v", entry.Path)
		}
		if err := prefixCreator.createChild(entry.Path, re_fuse.InitialNode{
			Directory: re_fuse.NewCASInitialContentsFetcher(
				context.Background(),
				cas.NewTreeDirectoryWalker(d.indexedTreeFetcher, childDigest),
				outputPathState.casFileFactory,
				instanceName),
		}); err != nil {
			return nil, util.StatusWrapf(err, "Failed to create directory %#v", entry.Path)
		}
	}

	// Create requested symbolic links.
	for _, entry := range request.Symlinks {
		if err := prefixCreator.createChild(entry.Path, re_fuse.InitialNode{
			Leaf: re_fuse.NewSymlink(entry.Target),
		}); err != nil {
			return nil, util.StatusWrapf(err, "Failed to create symbolic link %#v", entry.Path)
		}
	}

	return &emptypb.Empty{}, nil
}

// statWalker is an implementation of ScopeWalker and ComponentWalker
// that is used by BatchStat() to resolve the file or directory
// corresponding to a requested path. It is capable of expanding
// symbolic links, if encountered.
type statWalker struct {
	followSymlinks bool
	digestFunction *digest.Function

	stack      []re_fuse.PrepopulatedDirectory
	fileStatus *remoteoutputservice.FileStatus
}

func (cw *statWalker) OnScope(absolute bool) (path.ComponentWalker, error) {
	if absolute {
		cw.stack = cw.stack[:1]
	}
	// Currently in a known directory.
	cw.fileStatus = &remoteoutputservice.FileStatus{
		FileType: &remoteoutputservice.FileStatus_Directory{
			Directory: &emptypb.Empty{},
		},
	}
	return cw, nil
}

func (cw *statWalker) OnDirectory(name path.Component) (path.GotDirectoryOrSymlink, error) {
	d := cw.stack[len(cw.stack)-1]
	directory, leaf, err := d.LookupChild(name)
	if err != nil {
		return nil, err
	}

	if directory != nil {
		// Got a directory.
		cw.stack = append(cw.stack, directory)
		return path.GotDirectory{
			Child:        cw,
			IsReversible: true,
		}, nil
	}

	target, err := leaf.Readlink()
	if err == syscall.EINVAL {
		return nil, syscall.ENOTDIR
	} else if err != nil {
		return nil, err
	}

	// Got a symbolic link in the middle of a path. Those should
	// always be followed.
	cw.fileStatus = nil
	return path.GotSymlink{
		Parent: cw,
		Target: target,
	}, nil
}

func (cw *statWalker) OnTerminal(name path.Component) (*path.GotSymlink, error) {
	d := cw.stack[len(cw.stack)-1]
	directory, leaf, err := d.LookupChild(name)
	if err != nil {
		return nil, err
	}

	if directory != nil {
		// Got a directory. The existing FileStatus is sufficient.
		return nil, nil
	}

	if cw.followSymlinks {
		target, err := leaf.Readlink()
		if err == nil {
			// Got a symbolic link, and we should follow it.
			cw.fileStatus = nil
			return &path.GotSymlink{
				Parent: cw,
				Target: target,
			}, nil
		}
		if err != syscall.EINVAL {
			return nil, err
		}
	}

	fileStatus, err := leaf.GetOutputServiceFileStatus(cw.digestFunction)
	if err != nil {
		return nil, err
	}
	cw.fileStatus = fileStatus
	return nil, nil
}

func (cw *statWalker) OnUp() (path.ComponentWalker, error) {
	if len(cw.stack) == 1 {
		cw.fileStatus = nil
		return path.VoidComponentWalker, nil
	}
	cw.stack = cw.stack[:len(cw.stack)-1]
	return cw, nil
}

// BatchStat can be called by a build client to obtain the status of
// files and directories.
//
// Calling this method over gRPC may be far more efficient than
// obtaining this information through the FUSE file system, as batching
// significantly reduces the amount of context switching. It also
// prevents the computation of digests for files for which the digest is
// already known.
func (d *RemoteOutputServiceDirectory) BatchStat(ctx context.Context, request *remoteoutputservice.BatchStatRequest) (*remoteoutputservice.BatchStatResponse, error) {
	outputPathState, buildState, err := d.getOutputPathAndBuildState(request.BuildId)
	if err != nil {
		return nil, err
	}

	response := remoteoutputservice.BatchStatResponse{
		Responses: make([]*remoteoutputservice.StatResponse, 0, len(request.Paths)),
	}
	for _, statPath := range request.Paths {
		statWalker := statWalker{
			followSymlinks: request.FollowSymlinks,
			stack:          []re_fuse.PrepopulatedDirectory{outputPathState.rootDirectory},
		}
		if request.IncludeFileDigest {
			statWalker.digestFunction = &buildState.digestFunction
		}

		resolvedPath, scopeWalker := path.EmptyBuilder.Join(
			buildState.scopeWalkerFactory.New(path.NewLoopDetectingScopeWalker(&statWalker)))
		if err := path.Resolve(statPath, scopeWalker); err == syscall.ENOENT {
			// Path does not exist.
			response.Responses = append(response.Responses, &remoteoutputservice.StatResponse{})
		} else if err != nil {
			// Some other error occurred.
			return nil, util.StatusWrapf(err, "Failed to resolve path %#v beyond %#v", statPath, resolvedPath.String())
		} else if statWalker.fileStatus == nil {
			// Path resolves to a location outside the file
			// system. Return the resolved path back to the
			// client, so it can stat() it manually.
			response.Responses = append(response.Responses, &remoteoutputservice.StatResponse{
				FileStatus: &remoteoutputservice.FileStatus{
					FileType: &remoteoutputservice.FileStatus_External_{
						External: &remoteoutputservice.FileStatus_External{
							NextPath: resolvedPath.String(),
						},
					},
				},
			})
		} else {
			// Path resolved to a location inside the file system.
			// Return the stat response that was captured.
			response.Responses = append(response.Responses, &remoteoutputservice.StatResponse{
				FileStatus: statWalker.fileStatus,
			})
		}
	}
	return &response, nil
}

// FinalizeBuild can be called by a build client to indicate the current
// build has completed. This prevents successive BatchCreate() and
// BatchStat() calls from being processed.
func (d *RemoteOutputServiceDirectory) FinalizeBuild(ctx context.Context, request *remoteoutputservice.FinalizeBuildRequest) (*emptypb.Empty, error) {
	d.lock.Lock()
	defer d.lock.Unlock()

	// Silently ignore requests for unknown build IDs. This ensures
	// that FinalizeBuild() remains idempotent.
	if outputPathState, ok := d.buildIDs[request.BuildId]; ok {
		outputPathState.rootDirectory.FinalizeBuild()
		buildState := outputPathState.buildState
		delete(d.buildIDs, buildState.id)
		outputPathState.buildState = nil
	}
	return &emptypb.Empty{}, nil
}

// FUSEAccess checks the access rights of the root directory of the
// Remote Output Service.
func (d *RemoteOutputServiceDirectory) FUSEAccess(mask uint32) fuse.Status {
	if mask&^(fuse.R_OK|fuse.X_OK) != 0 {
		return fuse.EACCES
	}
	return fuse.OK
}

// FUSEGetAttr returns the attributes of the root directory of the
// Remote Output Service.
func (d *RemoteOutputServiceDirectory) FUSEGetAttr(out *fuse.Attr) {
	out.Ino = d.inodeNumber
	out.Mode = fuse.S_IFDIR | 0o555
	d.lock.Lock()
	out.Nlink = re_fuse.EmptyDirectoryLinkCount + uint32(len(d.outputBaseIDs))
	d.lock.Unlock()
}

// FUSELookup can be used to look up the root directory of an output
// path for a given output base.
func (d *RemoteOutputServiceDirectory) FUSELookup(name path.Component, out *fuse.Attr) (re_fuse.Directory, re_fuse.Leaf, fuse.Status) {
	d.lock.Lock()
	outputPathState, ok := d.outputBaseIDs[name]
	d.lock.Unlock()
	if !ok {
		return nil, nil, fuse.ENOENT
	}
	outputPathState.rootDirectory.FUSEGetAttr(out)
	return outputPathState.rootDirectory, nil, fuse.OK
}

// FUSEReadDir returns a list of all the output paths managed by this
// Remote Output Service.
func (d *RemoteOutputServiceDirectory) FUSEReadDir() ([]fuse.DirEntry, fuse.Status) {
	d.lock.Lock()
	defer d.lock.Unlock()

	entries := make([]fuse.DirEntry, 0, len(d.outputBaseIDs))
	for name, outputPathState := range d.outputBaseIDs {
		entries = append(entries, fuse.DirEntry{
			Mode: fuse.S_IFDIR,
			Ino:  outputPathState.rootDirectoryInodeNumber,
			Name: name.String(),
		})
	}
	return entries, fuse.OK
}

// FUSEReadDirPlus returns a list of all the output paths managed by
// this Remote Output Service.
func (d *RemoteOutputServiceDirectory) FUSEReadDirPlus() ([]re_fuse.DirectoryDirEntry, []re_fuse.LeafDirEntry, fuse.Status) {
	d.lock.Lock()
	defer d.lock.Unlock()

	entries := make([]re_fuse.DirectoryDirEntry, 0, len(d.outputBaseIDs))
	for name, outputPathState := range d.outputBaseIDs {
		entries = append(entries, re_fuse.DirectoryDirEntry{
			Child: outputPathState.rootDirectory,
			DirEntry: fuse.DirEntry{
				Mode: fuse.S_IFDIR,
				Ino:  outputPathState.rootDirectoryInodeNumber,
				Name: name.String(),
			},
		})
	}
	return entries, nil, fuse.OK
}
