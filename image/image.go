// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2014-2016 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package image

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/snapcore/snapd/arch"
	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/boot"
	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/overlord/auth"
	"github.com/snapcore/snapd/partition"
	"github.com/snapcore/snapd/progress"
	"github.com/snapcore/snapd/release"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/snap/squashfs"
	"github.com/snapcore/snapd/store"
)

var (
	Stdout io.Writer = os.Stdout
)

type Options struct {
	Snaps           []string
	RootDir         string
	Channel         string
	ModelFile       string
	GadgetUnpackDir string
}

func Prepare(opts *Options) error {
	if err := downloadUnpackGadget(opts); err != nil {
		return err
	}

	return bootstrapToRootDir(opts)
}

func decodeModelAssertion(fn string) (*asserts.Model, error) {
	rawAssert, err := ioutil.ReadFile(fn)
	if err != nil {
		return nil, fmt.Errorf("cannot read model assertion: %s", err)
	}

	ass, err := asserts.Decode(rawAssert)
	if err != nil {
		return nil, fmt.Errorf("cannot decode model assertion %q: %s", fn, err)
	}
	modela, ok := ass.(*asserts.Model)
	if !ok {
		return nil, fmt.Errorf("assertion in %q is not a model assertion", fn)
	}
	return modela, nil
}

func downloadUnpackGadget(opts *Options) error {
	model, err := decodeModelAssertion(opts.ModelFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(opts.GadgetUnpackDir, 0755); err != nil {
		return fmt.Errorf("cannot create gadget unpack dir %q: %s", opts.GadgetUnpackDir, err)
	}

	dlOpts := &downloadOptions{
		TargetDir:    opts.GadgetUnpackDir,
		Channel:      opts.Channel,
		StoreID:      model.Store(),
		Architecture: model.Architecture(),
	}
	snapFn, _, err := acquireSnap(model.Gadget(), dlOpts)
	if err != nil {
		return err
	}
	// FIXME: jumping through layers here, we need to make
	//        unpack part of the container interface (again)
	snap := squashfs.New(snapFn)
	return snap.Unpack("*", opts.GadgetUnpackDir)
}

func acquireSnap(snapName string, dlOpts *downloadOptions) (downloadedSnap string, info *snap.Info, err error) {
	// FIXME: add support for sideloading snaps here
	return downloadSnapWithSideInfo(snapName, dlOpts)
}

func bootstrapToRootDir(opts *Options) error {
	// FIXME: try to avoid doing this
	if opts.RootDir != "" {
		dirs.SetRootDir(opts.RootDir)
		defer dirs.SetRootDir("/")
	}

	// sanity check target
	if osutil.FileExists(dirs.SnapStateFile) {
		return fmt.Errorf("cannot bootstrap over existing system")
	}

	model, err := decodeModelAssertion(opts.ModelFile)
	if err != nil {
		return err
	}

	// put snaps in place
	if err := os.MkdirAll(dirs.SnapBlobDir, 0755); err != nil {
		return err
	}

	snapSeedDir := filepath.Join(dirs.SnapSeedDir, "snaps")
	dlOpts := &downloadOptions{
		TargetDir:    snapSeedDir,
		Channel:      opts.Channel,
		StoreID:      model.Store(),
		Architecture: model.Architecture(),
	}

	// FIXME: support sideloading snaps by copying the boostrap.snaps
	//        first and keeping track of the already downloaded names
	snaps := []string{}
	snaps = append(snaps, opts.Snaps...)
	snaps = append(snaps, model.Gadget())
	snaps = append(snaps, model.Core())
	snaps = append(snaps, model.Kernel())
	snaps = append(snaps, model.RequiredSnaps()...)

	for _, d := range []string{dirs.SnapBlobDir, snapSeedDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}
	}

	downloadedSnapsInfo := map[string]*snap.Info{}
	var seedYaml snap.Seed
	for _, snapName := range snaps {
		fmt.Fprintf(Stdout, "Fetching %s\n", snapName)
		fn, info, err := acquireSnap(snapName, dlOpts)
		if err != nil {
			return err
		}

		// kernel/os are required for booting
		if snapName == model.Kernel() || snapName == model.Core() {
			dst := filepath.Join(dirs.SnapBlobDir, filepath.Base(fn))
			if err := osutil.CopyFile(fn, dst, 0); err != nil {
				return err
			}
			// store the snap.Info for kernel/os so
			// that the bootload can DTRT
			downloadedSnapsInfo[dst] = info
		}

		// set seed.yaml
		seedYaml.Snaps = append(seedYaml.Snaps, &snap.SeedSnap{
			Name:        info.Name(),
			SnapID:      info.SnapID,
			Revision:    info.Revision,
			Channel:     info.Channel,
			DeveloperID: info.DeveloperID,
			Developer:   info.Developer,
			File:        filepath.Base(fn),
		})
	}

	seedFn := filepath.Join(dirs.SnapSeedDir, "seed.yaml")
	if err := seedYaml.Write(seedFn); err != nil {
		return fmt.Errorf("cannot write seed.yaml: %s", err)
	}

	// now do the bootloader stuff
	if err := partition.InstallBootConfig(opts.GadgetUnpackDir); err != nil {
		return err
	}

	if err := setBootvars(downloadedSnapsInfo); err != nil {
		return err
	}

	return nil
}

func setBootvars(downloadedSnapsInfo map[string]*snap.Info) error {
	// Set bootvars for kernel/core snaps so the system boots and
	// does the first-time initialization. There is also no
	// mounted kernel/core snap, but just the blobs.
	bootloader, err := partition.FindBootloader()
	if err != nil {
		return fmt.Errorf("cannot set kernel/core boot variables: %s", err)
	}

	snaps, err := filepath.Glob(filepath.Join(dirs.SnapBlobDir, "*.snap"))
	if len(snaps) == 0 || err != nil {
		return fmt.Errorf("internal error: cannot find core/kernel snap")
	}
	for _, fn := range snaps {
		bootvar := ""

		info := downloadedSnapsInfo[fn]
		switch info.Type {
		case snap.TypeOS:
			bootvar = "snap_core"
		case snap.TypeKernel:
			bootvar = "snap_kernel"
			if err := extractKernelAssets(fn, info); err != nil {
				return err
			}
		}

		if bootvar != "" {
			name := filepath.Base(fn)
			if err := bootloader.SetBootVar(bootvar, name); err != nil {
				return err
			}
		}
	}

	return nil
}

func runCommand(cmdStr ...string) error {
	cmd := exec.Command(cmdStr[0], cmdStr[1:]...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cannot run %v: %s", cmdStr, osutil.OutputErr(output, err))
	}
	return nil
}

func extractKernelAssets(snapPath string, info *snap.Info) error {
	snapf, err := snap.Open(snapPath)
	if err != nil {
		return err
	}

	if err := boot.ExtractKernelAssets(info, snapf); err != nil {
		return err
	}
	return nil
}

func copyLocalSnapFile(snapName, targetDir string) (copyiedSnapFn string, info *snap.Info, err error) {
	snapFile, err := snap.Open(snapName)
	if err != nil {
		return "", nil, err
	}
	info, err = snap.ReadInfoFromSnapFile(snapFile, nil)
	if err != nil {
		return "", nil, err
	}
	// local snap gets sideloaded revision
	if info.Revision.Unset() {
		info.Revision = snap.R(-1)
	}
	dst := filepath.Join(targetDir, filepath.Dir(info.MountFile()))

	return dst, info, osutil.CopyFile(snapName, dst, 0)
}

type downloadOptions struct {
	Series       string
	TargetDir    string
	StoreID      string
	Channel      string
	Architecture string
}

// replaced in the tests
var storeNew = func(storeID string) Store {
	return store.New(nil, storeID, nil)
}

type Store interface {
	Snap(name, channel string, devmode bool, user *auth.UserState) (*snap.Info, error)
	Download(name string, downloadInfo *snap.DownloadInfo, pbar progress.Meter, user *auth.UserState) (path string, err error)
}

func downloadSnapWithSideInfo(name string, opts *downloadOptions) (targetPath string, info *snap.Info, err error) {
	if opts == nil {
		opts = &downloadOptions{}
	}

	// FIXME: avoid global mutation
	if opts.Series != "" {
		oldSeries := release.Series
		defer func() { release.Series = oldSeries }()

		release.Series = opts.Series
	}
	// FIXME: avoid global mutation
	if opts.Architecture != "" {
		oldArchitecture := arch.UbuntuArchitecture()
		defer func() { arch.SetArchitecture(arch.ArchitectureType(oldArchitecture)) }()

		arch.SetArchitecture(arch.ArchitectureType(opts.Architecture))
	}

	storeID := opts.StoreID
	if storeID == "canonical" {
		storeID = ""
	}

	targetDir := opts.TargetDir
	if targetDir == "" {
		pwd, err := os.Getwd()
		if err != nil {
			return "", nil, err
		}
		targetDir = pwd
	}

	m := storeNew(storeID)
	snap, err := m.Snap(name, opts.Channel, false, nil)
	if err != nil {
		return "", nil, fmt.Errorf("cannot find snap %q: %s", name, err)
	}
	pb := progress.NewTextProgress()
	tmpName, err := m.Download(name, &snap.DownloadInfo, pb, nil)
	if err != nil {
		return "", nil, err
	}
	defer os.Remove(tmpName)

	baseName := filepath.Base(snap.MountFile())
	targetPath = filepath.Join(targetDir, baseName)
	if err := osutil.CopyFile(tmpName, targetPath, 0); err != nil {
		return "", nil, err
	}

	return targetPath, snap, nil
}
