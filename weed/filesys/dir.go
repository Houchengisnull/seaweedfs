package filesys

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/chrislusf/seaweedfs/weed/filer2"
	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/fuse"
	"github.com/seaweedfs/fuse/fs"
)

type Dir struct {
	Path       string
	wfs        *WFS
	attributes *filer_pb.FuseAttributes
}

var _ = fs.Node(&Dir{})
var _ = fs.NodeCreater(&Dir{})
var _ = fs.NodeMkdirer(&Dir{})
var _ = fs.NodeRequestLookuper(&Dir{})
var _ = fs.HandleReadDirAller(&Dir{})
var _ = fs.NodeRemover(&Dir{})
var _ = fs.NodeRenamer(&Dir{})
var _ = fs.NodeSetattrer(&Dir{})

func (dir *Dir) Attr(ctx context.Context, attr *fuse.Attr) error {

	// https://github.com/bazil/fuse/issues/196
	attr.Valid = time.Second

	if dir.Path == dir.wfs.option.FilerMountRootPath {
		attr.Uid = dir.wfs.option.MountUid
		attr.Gid = dir.wfs.option.MountGid
		attr.Mode = dir.wfs.option.MountMode
		return nil
	}

	item := dir.wfs.listDirectoryEntriesCache.Get(dir.Path)
	if item != nil && !item.Expired() {
		entry := item.Value().(*filer_pb.Entry)

		attr.Mtime = time.Unix(entry.Attributes.Mtime, 0)
		attr.Ctime = time.Unix(entry.Attributes.Crtime, 0)
		attr.Mode = os.FileMode(entry.Attributes.FileMode)
		attr.Gid = entry.Attributes.Gid
		attr.Uid = entry.Attributes.Uid

		return nil
	}

	parent, name := filepath.Split(dir.Path)

	err := dir.wfs.withFilerClient(ctx, func(client filer_pb.SeaweedFilerClient) error {

		request := &filer_pb.LookupDirectoryEntryRequest{
			Directory: parent,
			Name:      name,
		}

		glog.V(1).Infof("read dir %s request: %v", dir.Path, request)
		resp, err := client.LookupDirectoryEntry(ctx, request)
		if err != nil {
			if err == filer2.ErrNotFound {
				return nil
			}
			glog.V(0).Infof("read dir %s attr %v: %v", dir.Path, request, err)
			return err
		}

		if resp.Entry != nil {
			dir.attributes = resp.Entry.Attributes
		}
		glog.V(2).Infof("read dir %s attr: %v", dir.Path, dir.attributes)

		// dir.wfs.listDirectoryEntriesCache.Set(dir.Path, resp.Entry, dir.wfs.option.EntryCacheTtl)

		return nil
	})

	if err != nil {
		glog.V(2).Infof("read dir %s attr: %v, error: %v", dir.Path, dir.attributes, err)
		return err
	}

	glog.V(2).Infof("dir %s: %v perm: %v", dir.Path, dir.attributes, os.FileMode(dir.attributes.FileMode))

	attr.Mode = os.FileMode(dir.attributes.FileMode) | os.ModeDir

	attr.Mtime = time.Unix(dir.attributes.Mtime, 0)
	attr.Ctime = time.Unix(dir.attributes.Crtime, 0)
	attr.Gid = dir.attributes.Gid
	attr.Uid = dir.attributes.Uid

	return nil
}

func (dir *Dir) newFile(name string, entry *filer_pb.Entry) *File {
	return &File{
		Name:           name,
		dir:            dir,
		wfs:            dir.wfs,
		entry:          entry,
		entryViewCache: nil,
	}
}

func (dir *Dir) Create(ctx context.Context, req *fuse.CreateRequest,
	resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {

	request := &filer_pb.CreateEntryRequest{
		Directory: dir.Path,
		Entry: &filer_pb.Entry{
			Name:        req.Name,
			IsDirectory: req.Mode&os.ModeDir > 0,
			Attributes: &filer_pb.FuseAttributes{
				Mtime:       time.Now().Unix(),
				Crtime:      time.Now().Unix(),
				FileMode:    uint32(req.Mode),
				Uid:         req.Uid,
				Gid:         req.Gid,
				Collection:  dir.wfs.option.Collection,
				Replication: dir.wfs.option.Replication,
				TtlSec:      dir.wfs.option.TtlSec,
			},
		},
	}
	glog.V(1).Infof("create: %v", request)

	if request.Entry.IsDirectory {
		if err := dir.wfs.withFilerClient(ctx, func(client filer_pb.SeaweedFilerClient) error {
			if _, err := client.CreateEntry(ctx, request); err != nil {
				glog.V(0).Infof("create %s/%s: %v", dir.Path, req.Name, err)
				return fuse.EIO
			}
			return nil
		}); err != nil {
			return nil, nil, err
		}
	}

	file := dir.newFile(req.Name, request.Entry)
	if !request.Entry.IsDirectory {
		file.isOpen = true
	}
	fh := dir.wfs.AcquireHandle(file, req.Uid, req.Gid)
	fh.dirtyMetadata = true
	return file, fh, nil

}

func (dir *Dir) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {

	err := dir.wfs.withFilerClient(ctx, func(client filer_pb.SeaweedFilerClient) error {

		request := &filer_pb.CreateEntryRequest{
			Directory: dir.Path,
			Entry: &filer_pb.Entry{
				Name:        req.Name,
				IsDirectory: true,
				Attributes: &filer_pb.FuseAttributes{
					Mtime:    time.Now().Unix(),
					Crtime:   time.Now().Unix(),
					FileMode: uint32(req.Mode),
					Uid:      req.Uid,
					Gid:      req.Gid,
				},
			},
		}

		glog.V(1).Infof("mkdir: %v", request)
		if _, err := client.CreateEntry(ctx, request); err != nil {
			glog.V(0).Infof("mkdir %s/%s: %v", dir.Path, req.Name, err)
			return fuse.EIO
		}

		return nil
	})

	if err == nil {
		node := &Dir{Path: path.Join(dir.Path, req.Name), wfs: dir.wfs}
		return node, nil
	}

	return nil, err
}

func (dir *Dir) Lookup(ctx context.Context, req *fuse.LookupRequest, resp *fuse.LookupResponse) (node fs.Node, err error) {

	var entry *filer_pb.Entry

	item := dir.wfs.listDirectoryEntriesCache.Get(path.Join(dir.Path, req.Name))
	if item != nil && !item.Expired() {
		entry = item.Value().(*filer_pb.Entry)
	}

	if entry == nil {
		err = dir.wfs.withFilerClient(ctx, func(client filer_pb.SeaweedFilerClient) error {

			request := &filer_pb.LookupDirectoryEntryRequest{
				Directory: dir.Path,
				Name:      req.Name,
			}

			glog.V(4).Infof("lookup directory entry: %v", request)
			resp, err := client.LookupDirectoryEntry(ctx, request)
			if err != nil {
				// glog.V(0).Infof("lookup %s/%s: %v", dir.Path, name, err)
				return fuse.ENOENT
			}

			entry = resp.Entry

			// dir.wfs.listDirectoryEntriesCache.Set(path.Join(dir.Path, entry.Name), entry, dir.wfs.option.EntryCacheTtl)

			return nil
		})
	}

	if entry != nil {
		if entry.IsDirectory {
			node = &Dir{Path: path.Join(dir.Path, req.Name), wfs: dir.wfs, attributes: entry.Attributes}
		} else {
			node = dir.newFile(req.Name, entry)
		}

		resp.EntryValid = time.Duration(0)
		resp.Attr.Mtime = time.Unix(entry.Attributes.Mtime, 0)
		resp.Attr.Ctime = time.Unix(entry.Attributes.Crtime, 0)
		resp.Attr.Mode = os.FileMode(entry.Attributes.FileMode)
		resp.Attr.Gid = entry.Attributes.Gid
		resp.Attr.Uid = entry.Attributes.Uid

		return node, nil
	}

	return nil, fuse.ENOENT
}

func (dir *Dir) ReadDirAll(ctx context.Context) (ret []fuse.Dirent, err error) {

	err = dir.wfs.withFilerClient(ctx, func(client filer_pb.SeaweedFilerClient) error {

		paginationLimit := 1024
		remaining := dir.wfs.option.DirListingLimit

		lastEntryName := ""

		for remaining >= 0 {

			request := &filer_pb.ListEntriesRequest{
				Directory:         dir.Path,
				StartFromFileName: lastEntryName,
				Limit:             uint32(paginationLimit),
			}

			glog.V(4).Infof("read directory: %v", request)
			resp, err := client.ListEntries(ctx, request)
			if err != nil {
				glog.V(0).Infof("list %s: %v", dir.Path, err)
				return fuse.EIO
			}

			cacheTtl := estimatedCacheTtl(len(resp.Entries))

			for _, entry := range resp.Entries {
				if entry.IsDirectory {
					dirent := fuse.Dirent{Name: entry.Name, Type: fuse.DT_Dir}
					ret = append(ret, dirent)
				} else {
					dirent := fuse.Dirent{Name: entry.Name, Type: fuse.DT_File}
					ret = append(ret, dirent)
				}
				dir.wfs.listDirectoryEntriesCache.Set(path.Join(dir.Path, entry.Name), entry, cacheTtl)
				lastEntryName = entry.Name
			}

			remaining -= len(resp.Entries)

			if len(resp.Entries) < paginationLimit {
				break
			}

		}

		return nil
	})

	return ret, err
}

func (dir *Dir) Remove(ctx context.Context, req *fuse.RemoveRequest) error {

	if !req.Dir {
		return dir.removeOneFile(ctx, req)
	}

	return dir.removeFolder(ctx, req)

}

func (dir *Dir) removeOneFile(ctx context.Context, req *fuse.RemoveRequest) error {

	var entry *filer_pb.Entry
	err := dir.wfs.withFilerClient(ctx, func(client filer_pb.SeaweedFilerClient) error {

		request := &filer_pb.LookupDirectoryEntryRequest{
			Directory: dir.Path,
			Name:      req.Name,
		}

		glog.V(4).Infof("lookup to-be-removed entry: %v", request)
		resp, err := client.LookupDirectoryEntry(ctx, request)
		if err != nil {
			// glog.V(0).Infof("lookup %s/%s: %v", dir.Path, name, err)
			return fuse.ENOENT
		}

		entry = resp.Entry

		return nil
	})

	if err != nil {
		return err
	}

	dir.wfs.deleteFileChunks(ctx, entry.Chunks)

	return dir.wfs.withFilerClient(ctx, func(client filer_pb.SeaweedFilerClient) error {

		request := &filer_pb.DeleteEntryRequest{
			Directory:    dir.Path,
			Name:         req.Name,
			IsDeleteData: false,
		}

		glog.V(3).Infof("remove file: %v", request)
		_, err := client.DeleteEntry(ctx, request)
		if err != nil {
			glog.V(3).Infof("remove file %s/%s: %v", dir.Path, req.Name, err)
			return fuse.ENOENT
		}

		dir.wfs.listDirectoryEntriesCache.Delete(path.Join(dir.Path, req.Name))

		return nil
	})

}

func (dir *Dir) removeFolder(ctx context.Context, req *fuse.RemoveRequest) error {

	return dir.wfs.withFilerClient(ctx, func(client filer_pb.SeaweedFilerClient) error {

		request := &filer_pb.DeleteEntryRequest{
			Directory:    dir.Path,
			Name:         req.Name,
			IsDeleteData: true,
		}

		glog.V(3).Infof("remove directory entry: %v", request)
		_, err := client.DeleteEntry(ctx, request)
		if err != nil {
			glog.V(3).Infof("remove %s/%s: %v", dir.Path, req.Name, err)
			return fuse.ENOENT
		}

		dir.wfs.listDirectoryEntriesCache.Delete(path.Join(dir.Path, req.Name))

		return nil
	})

}

func (dir *Dir) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {

	if dir.attributes == nil {
		return nil
	}

	glog.V(3).Infof("%v dir setattr %+v, fh=%d", dir.Path, req, req.Handle)
	if req.Valid.Mode() {
		dir.attributes.FileMode = uint32(req.Mode)
	}

	if req.Valid.Uid() {
		dir.attributes.Uid = req.Uid
	}

	if req.Valid.Gid() {
		dir.attributes.Gid = req.Gid
	}

	if req.Valid.Mtime() {
		dir.attributes.Mtime = req.Mtime.Unix()
	}

	parentDir, name := filer2.FullPath(dir.Path).DirAndName()
	return dir.wfs.withFilerClient(ctx, func(client filer_pb.SeaweedFilerClient) error {

		request := &filer_pb.UpdateEntryRequest{
			Directory: parentDir,
			Entry: &filer_pb.Entry{
				Name:       name,
				Attributes: dir.attributes,
			},
		}

		glog.V(1).Infof("set attr directory entry: %v", request)
		_, err := client.UpdateEntry(ctx, request)
		if err != nil {
			glog.V(0).Infof("UpdateEntry %s: %v", dir.Path, err)
			return fuse.EIO
		}

		dir.wfs.listDirectoryEntriesCache.Delete(dir.Path)

		return nil
	})

}

func estimatedCacheTtl(numEntries int) time.Duration {
	if numEntries < 100 {
		// 30 ms per entry
		return 3 * time.Second
	}
	if numEntries < 1000 {
		// 10 ms per entry
		return 10 * time.Second
	}
	if numEntries < 10000 {
		// 10 ms per entry
		return 100 * time.Second
	}

	// 2 ms per entry
	return time.Duration(numEntries*2) * time.Millisecond
}
