// Copyright 2018 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package build

import (
	"archive/tar"
	"context"
	"io"
	"io/ioutil"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

func TestGoBuildIsSupportedRef(t *testing.T) {
	base, err := random.Image(1024, 3)
	if err != nil {
		t.Fatalf("random.Image() = %v", err)
	}

	ng, err := NewGo(WithBaseImages(func(string) (v1.Image, error) { return base, nil }))
	if err != nil {
		t.Fatalf("NewGo() = %v", err)
	}
	ngstrict, err := NewGo(WithBaseImages(func(string) (v1.Image, error) { return base, nil }), WithStrictMode())
	if err != nil {
		t.Fatalf("NewGo() = %v", err)
	}

	tests := []struct {
		desc        string
		giveIp      string
		giveBuilder Interface
		wantSr      bool
	}{
		{
			desc:        "IP has main and was not defined as strict ref",
			giveIp:      "github.com/google/ko/cmd/ko",
			giveBuilder: ng,
			wantSr:      true,
		},
		{
			desc:        "IP has main and was defined as strict ref",
			giveIp:      "ko://github.com/google/ko/cmd/ko",
			giveBuilder: ngstrict,
			wantSr:      true},
		{
			desc:        "IP has neither main nor test",
			giveIp:      "github.com/google/ko/pkg/commands/options",
			giveBuilder: ngstrict,
			wantSr:      false,
		},
		{
			desc:        "ip doesnt exist",
			giveIp:      "github.com/google/ko/pkg/idontexist",
			giveBuilder: ngstrict,
			wantSr:      false,
		},
		{
			desc:        "ip has only tests but was not a strict ref",
			giveIp:      "github.com/google/ko/pkg/build",
			giveBuilder: ng,
			wantSr:      false,
		},
		{
			desc:        "ip has only test and was a strict ref",
			giveIp:      "ko://github.com/google/ko/pkg/build",
			giveBuilder: ngstrict,
			wantSr:      false,
		},
		{
			desc:        "ip has only test and was a strict test-ref",
			giveIp:      "ko-test://github.com/google/ko/pkg/build",
			giveBuilder: ngstrict,
			wantSr:      true,
		},
	}

	for _, ts := range tests {
		ip := filepath.FromSlash(ts.giveIp)
		t.Run(ts.desc, func(t *testing.T) {
			if ts.giveBuilder.IsSupportedReference(ip) != ts.wantSr {
				t.Errorf("IsSupportedReference(%q) = %v, want %v", ip, !ts.wantSr, ts.wantSr)
			}
		})
	}
}

func TestGoBuildIsSupportedRefWithModules(t *testing.T) {
	base, err := random.Image(1024, 3)
	if err != nil {
		t.Fatalf("random.Image() = %v", err)
	}
	mod := &modInfo{
		Path: filepath.FromSlash("github.com/google/ko/cmd/ko/test"),
		Dir:  ".",
	}

	ng, err := NewGo(WithBaseImages(func(string) (v1.Image, error) { return base, nil }), withModuleInfo(mod))
	if err != nil {
		t.Fatalf("NewGo() = %v", err)
	}

	// Supported import paths.
	for _, importpath := range []string{
		filepath.FromSlash("github.com/google/ko/cmd/ko/test"), // ko can build the test package.
	} {
		t.Run(importpath, func(t *testing.T) {
			if !ng.IsSupportedReference(importpath) {
				t.Errorf("IsSupportedReference(%q) = false, want true", importpath)
			}
		})
	}

	// Unsupported import paths.
	for _, importpath := range []string{
		filepath.FromSlash("github.com/google/ko/pkg/build"),       // not a command.
		filepath.FromSlash("github.com/google/ko/pkg/nonexistent"), // does not exist.
		filepath.FromSlash("github.com/google/ko/cmd/ko"),          // not in this module.
	} {
		t.Run(importpath, func(t *testing.T) {
			if ng.IsSupportedReference(importpath) {
				t.Errorf("IsSupportedReference(%v) = true, want false", importpath)
			}
		})
	}
}

// A helper method we use to substitute for the default "build" method.
func writeTempFile(_ context.Context, s string, _ v1.Platform, _ bool, _ bool) (string, error) {
	tmpDir, err := ioutil.TempDir("", "ko")
	if err != nil {
		return "", err
	}

	file, err := ioutil.TempFile(tmpDir, "out")
	if err != nil {
		return "", err
	}
	defer file.Close()
	if _, err := file.WriteString(filepath.ToSlash(s)); err != nil {
		return "", err
	}
	return file.Name(), nil
}

func TestGoBuildNoKoData(t *testing.T) {
	baseLayers := int64(3)
	base, err := random.Image(1024, baseLayers)
	if err != nil {
		t.Fatalf("random.Image() = %v", err)
	}
	importpath := "github.com/google/ko"

	creationTime := v1.Time{time.Unix(5000, 0)}
	ng, err := NewGo(
		WithCreationTime(creationTime),
		WithBaseImages(func(string) (v1.Image, error) { return base, nil }),
		withBuilder(writeTempFile),
	)
	if err != nil {
		t.Fatalf("NewGo() = %v", err)
	}

	img, err := ng.Build(context.Background(), filepath.Join(importpath, "cmd", "ko"))
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}

	ls, err := img.Layers()
	if err != nil {
		t.Fatalf("Layers() = %v", err)
	}

	// Check that we have the expected number of layers.
	t.Run("check layer count", func(t *testing.T) {
		// We get a layer for the go binary and a layer for the kodata/
		if got, want := int64(len(ls)), baseLayers+2; got != want {
			t.Fatalf("len(Layers()) = %v, want %v", got, want)
		}
	})

	// Check that rebuilding the image again results in the same image digest.
	t.Run("check determinism", func(t *testing.T) {
		expectedHash := v1.Hash{
			Algorithm: "sha256",
			Hex:       "fb82c95fc73eaf26d0b18b1bc2d23ee32059e46806a83a313e738aac4d039492",
		}
		appLayer := ls[baseLayers+1]

		if got, err := appLayer.Digest(); err != nil {
			t.Errorf("Digest() = %v", err)
		} else if got != expectedHash {
			t.Errorf("Digest() = %v, want %v", got, expectedHash)
		}
	})

	// Check that the entrypoint of the image is configured to invoke our Go application
	t.Run("check entrypoint", func(t *testing.T) {
		cfg, err := img.ConfigFile()
		if err != nil {
			t.Errorf("ConfigFile() = %v", err)
		}
		entrypoint := cfg.Config.Entrypoint
		if got, want := len(entrypoint), 1; got != want {
			t.Errorf("len(entrypoint) = %v, want %v", got, want)
		}

		if got, want := entrypoint[0], "/ko-app/ko"; got != want {
			t.Errorf("entrypoint = %v, want %v", got, want)
		}
	})

	t.Run("check creation time", func(t *testing.T) {
		cfg, err := img.ConfigFile()
		if err != nil {
			t.Errorf("ConfigFile() = %v", err)
		}

		actual := cfg.Created
		if actual.Time != creationTime.Time {
			t.Errorf("created = %v, want %v", actual, creationTime)
		}
	})
}

func TestGoBuild(t *testing.T) {
	baseLayers := int64(3)
	base, err := random.Image(1024, baseLayers)
	if err != nil {
		t.Fatalf("random.Image() = %v", err)
	}
	importpath := "github.com/google/ko"

	creationTime := v1.Time{time.Unix(5000, 0)}
	ng, err := NewGo(
		WithCreationTime(creationTime),
		WithBaseImages(func(string) (v1.Image, error) { return base, nil }),
		withBuilder(writeTempFile),
	)
	if err != nil {
		t.Fatalf("NewGo() = %v", err)
	}

	img, err := ng.Build(context.Background(), filepath.Join(importpath, "cmd", "ko", "test"))
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}

	ls, err := img.Layers()
	if err != nil {
		t.Fatalf("Layers() = %v", err)
	}

	// Check that we have the expected number of layers.
	t.Run("check layer count", func(t *testing.T) {
		// We get a layer for the go binary and a layer for the kodata/
		if got, want := int64(len(ls)), baseLayers+2; got != want {
			t.Fatalf("len(Layers()) = %v, want %v", got, want)
		}
	})

	// Check that rebuilding the image again results in the same image digest.
	t.Run("check determinism", func(t *testing.T) {
		expectedHash := v1.Hash{
			Algorithm: "sha256",
			Hex:       "4c7f97dda30576670c3a8967424f7dea023030bb3df74fc4bd10329bcb266fc2",
		}
		appLayer := ls[baseLayers+1]

		if got, err := appLayer.Digest(); err != nil {
			t.Errorf("Digest() = %v", err)
		} else if got != expectedHash {
			t.Errorf("Digest() = %v, want %v", got, expectedHash)
		}
	})

	t.Run("check app layer contents", func(t *testing.T) {
		dataLayer := ls[baseLayers]

		if _, err := dataLayer.Digest(); err != nil {
			t.Errorf("Digest() = %v", err)
		}
		// We don't check the data layer here because it includes a symlink of refs and
		// will produce a distinct hash each time we commit something.

		r, err := dataLayer.Uncompressed()
		if err != nil {
			t.Errorf("Uncompressed() = %v", err)
		}
		defer r.Close()
		tr := tar.NewReader(r)
		if _, err := tr.Next(); err == io.EOF {
			t.Errorf("Layer contained no files")
		}
	})

	// Check that the kodata layer contains the expected data (even though it was a symlink
	// outside kodata).
	t.Run("check kodata", func(t *testing.T) {
		dataLayer := ls[baseLayers]
		r, err := dataLayer.Uncompressed()
		if err != nil {
			t.Errorf("Uncompressed() = %v", err)
		}
		defer r.Close()
		found := false
		tr := tar.NewReader(r)
		for {
			header, err := tr.Next()
			if err == io.EOF {
				break
			} else if err != nil {
				t.Errorf("Next() = %v", err)
				continue
			}
			if header.Name != filepath.Join(kodataRoot, "kenobi") {
				continue
			}
			found = true
			body, err := ioutil.ReadAll(tr)
			if err != nil {
				t.Errorf("ReadAll() = %v", err)
			} else if want, got := "Hello there\n", string(body); got != want {
				t.Errorf("ReadAll() = %v, wanted %v", got, want)
			}
		}
		if !found {
			t.Error("Didn't find expected file in tarball")
		}
	})

	// Check that the entrypoint of the image is configured to invoke our Go application
	t.Run("check entrypoint", func(t *testing.T) {
		cfg, err := img.ConfigFile()
		if err != nil {
			t.Errorf("ConfigFile() = %v", err)
		}
		entrypoint := cfg.Config.Entrypoint
		if got, want := len(entrypoint), 1; got != want {
			t.Errorf("len(entrypoint) = %v, want %v", got, want)
		}

		if got, want := entrypoint[0], "/ko-app/test"; got != want {
			t.Errorf("entrypoint = %v, want %v", got, want)
		}
	})

	// Check that the environment contains the KO_DATA_PATH environment variable.
	t.Run("check KO_DATA_PATH env var", func(t *testing.T) {
		cfg, err := img.ConfigFile()
		if err != nil {
			t.Errorf("ConfigFile() = %v", err)
		}
		found := false
		for _, entry := range cfg.Config.Env {
			if entry == "KO_DATA_PATH="+kodataRoot {
				found = true
			}
		}
		if !found {
			t.Error("Didn't find expected file in tarball.")
		}
	})

	t.Run("check creation time", func(t *testing.T) {
		cfg, err := img.ConfigFile()
		if err != nil {
			t.Errorf("ConfigFile() = %v", err)
		}

		actual := cfg.Created
		if actual.Time != creationTime.Time {
			t.Errorf("created = %v, want %v", actual, creationTime)
		}
	})
}
