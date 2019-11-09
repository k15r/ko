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

package commands

import (
	"context"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/ko/pkg/build"
	"github.com/google/ko/pkg/commands/options"
	"github.com/google/ko/pkg/publish"
	"github.com/google/ko/pkg/resolve"
	"github.com/mattmoor/dep-notify/pkg/graph"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/labels"
)

func gobuildOptions(bo *options.BuildOptions, so *options.StrictOptions) ([]build.Option, error) {
	creationTime, err := getCreationTime()
	if err != nil {
		return nil, err
	}
	opts := []build.Option{
		build.WithBaseImages(getBaseImage),
	}
	if creationTime != nil {
		opts = append(opts, build.WithCreationTime(*creationTime))
	}
	if bo.DisableOptimizations {
		opts = append(opts, build.WithDisabledOptimizations())
	}
	if so.Strict {
		opts = append(opts, build.WithStrictMode( ))
	}
	return opts, nil
}

func makeBuilder(bo *options.BuildOptions, so *options.StrictOptions) (*build.Caching, error) {
	opt, err := gobuildOptions(bo, so)
	if err != nil {
		log.Fatalf("error setting up builder options: %v", err)
	}
	innerBuilder, err := build.NewGo(opt...)
	if err != nil {
		return nil, err
	}

	innerBuilder = build.NewLimiter(innerBuilder, bo.ConcurrentBuilds)

	// tl;dr Wrap builder in a caching builder.
	//
	// The caching builder should on Build calls:
	//  - Check for a valid Build future
	//    - if a valid Build future exists at the time of the request,
	//      then block on it.
	//    - if it does not, then initiate and record a Build future.
	//  - When import paths are "affected" by filesystem changes during a
	//    Watch, then invalidate their build futures *before* we put the
	//    affected yaml files onto the channel
	//
	// This will benefit the following key cases:
	// 1. When the same import path is referenced across multiple yaml files
	//    we can elide subsequent builds by blocking on the same image future.
	// 2. When an affected yaml file has multiple import paths (mostly unaffected)
	//    we can elide the builds of unchanged import paths.
	return build.NewCaching(innerBuilder)
}

func makePublisher(no *options.NameOptions, lo *options.LocalOptions, ta *options.TagsOptions) (publish.Interface, error) {
	// Create the publish.Interface that we will use to publish image references
	// to either a docker daemon or a container image registry.
	innerPublisher, err := func() (publish.Interface, error) {
		namer := options.MakeNamer(no)

		repoName := os.Getenv("KO_DOCKER_REPO")
		if lo.Local || repoName == publish.LocalDomain {
			return publish.NewDaemon(namer, ta.Tags), nil
		}
		if repoName == "" {
			return nil, errors.New("KO_DOCKER_REPO environment variable is unset")
		}
		if _, err := name.NewRegistry(repoName); err != nil {
			if _, err := name.NewRepository(repoName); err != nil {
				return nil, fmt.Errorf("failed to parse environment variable KO_DOCKER_REPO=%q as repository: %v", repoName, err)
			}
		}

		return publish.NewDefault(repoName,
			publish.WithAuthFromKeychain(authn.DefaultKeychain),
			publish.WithNamer(namer),
			publish.WithTags(ta.Tags),
			publish.Insecure(lo.InsecureRegistry))
	}()
	if err != nil {
		return nil, err
	}

	// Wrap publisher in a memoizing publisher implementation.
	return publish.NewCaching(innerPublisher)
}

// resolvedFuture represents a "future" for the bytes of a resolved file.
type resolvedFuture chan []byte

func resolveFilesToWriter(
	ctx context.Context, 
	builder *build.Caching,
	publisher publish.Interface,
	fo *options.FilenameOptions,
	so *options.SelectorOptions,
	sto *options.StrictOptions,
	out io.WriteCloser) {
	defer out.Close()

	// By having this as a channel, we can hook this up to a filesystem
	// watcher and leave `fs` open to stream the names of yaml files
	// affected by code changes (including the modification of existing or
	// creation of new yaml files).
	fs := options.EnumerateFiles(fo)

	// This tracks filename -> []importpath
	var sm sync.Map

	var g graph.Interface
	var errCh chan error
	var err error
	if fo.Watch {
		// Start a dep-notify process that on notifications scans the
		// file-to-recorded-build map and for each affected file resends
		// the filename along the channel.
		g, errCh, err = graph.New(func(ss graph.StringSet) {
			sm.Range(func(k, v interface{}) bool {
				key := k.(string)
				value := v.([]string)

				for _, ip := range value {
					if ss.Has(ip) {
						// See the comment above about how "builder" works.
						builder.Invalidate(ip)
						fs <- key
					}
				}
				return true
			})
		})
		if err != nil {
			log.Fatalf("Error creating dep-notify graph: %v", err)
		}
		// Cleanup the fsnotify hooks when we're done.
		defer g.Shutdown()
	}

	var futures []resolvedFuture
	for {
		// Each iteration, if there is anything in the list of futures,
		// listen to it in addition to the file enumerating channel.
		// A nil channel is never available to receive on, so if nothing
		// is available, this will result in us exclusively selecting
		// on the file enumerating channel.
		var bf resolvedFuture
		if len(futures) > 0 {
			bf = futures[0]
		} else if fs == nil {
			// There are no more files to enumerate and the futures
			// have been drained, so quit.
			break
		}

		select {
		case f, ok := <-fs:
			if !ok {
				// a nil channel is never available to receive on.
				// This allows us to drain the list of in-process
				// futures without this case of the select winning
				// each time.
				fs = nil
				break
			}

			// Make a new future to use to ship the bytes back and append
			// it to the list of futures (see comment below about ordering).
			ch := make(resolvedFuture)
			futures = append(futures, ch)

			// Kick off the resolution that will respond with its bytes on
			// the future.
			go func(f string) {
				defer close(ch)
				// Record the builds we do via this builder.
				recordingBuilder := &build.Recorder{
					Builder: builder,
				}
				b, err := resolveFile(ctx, f, recordingBuilder, publisher, so, sto)
				if err != nil {
					// Don't let build errors disrupt the watch.
					lg := log.Fatalf
					if fo.Watch {
						lg = log.Printf
					}
					lg("error processing import paths in %q: %v", f, err)
					return
				}
				// Associate with this file the collection of binary import paths.
				sm.Store(f, recordingBuilder.ImportPaths)
				ch <- b
				if fo.Watch {
					for _, ip := range recordingBuilder.ImportPaths {
						// Technically we never remove binary targets from the graph,
						// which will increase our graph's watch load, but the
						// notifications that they change will result in no affected
						// yamls, and no new builds or deploys.
						if err := g.Add(ip); err != nil {
							log.Fatalf("Error adding importpath to dep graph: %v", err)
						}
					}
				}
			}(f)

		case b, ok := <-bf:
			// Once the head channel returns something, dequeue it.
			// We listen to the futures in order to be respectful of
			// the kubectl apply ordering, which matters!
			futures = futures[1:]
			if ok {
				// Write the next body and a trailing delimiter.
				// We write the delimeter LAST so that when streamed to
				// kubectl it knows that the resource is complete and may
				// be applied.
				out.Write(append(b, []byte("\n---\n")...))
			}

		case err := <-errCh:
			log.Fatalf("Error watching dependencies: %v", err)
		}
	}
}

func resolveFile(
	ctx context.Context,
	f string,
	builder build.Interface,
	pub publish.Interface,
	so *options.SelectorOptions,
	sto *options.StrictOptions) (b []byte, err error) {

	var selector labels.Selector
	if so.Selector != "" {
		var err error
		selector, err = labels.Parse(so.Selector)

		if err != nil {
			return nil, fmt.Errorf("unable to parse selector: %v", err)
		}
	}

	if f == "-" {
		b, err = ioutil.ReadAll(os.Stdin)
	} else {
		b, err = ioutil.ReadFile(f)
	}
	if err != nil {
		return nil, err
	}

	var docNodes []*yaml.Node

	// The loop is to support multi-document yaml files.
	// This is handled by using a yaml.Decoder and reading objects until io.EOF, see:
	// https://godoc.org/gopkg.in/yaml.v3#Decoder.Decode
	decoder := yaml.NewDecoder(bytes.NewBuffer(b))
	for {
		var doc yaml.Node
		if err := decoder.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		if selector != nil {
			if match, err := resolve.MatchesSelector(&doc, selector); err != nil {
				return nil, fmt.Errorf("error evaluating selector: %v", err)
			} else if !match {
				continue
			}
		}

		docNodes = append(docNodes, &doc)

	}

	if err := resolve.ImageReferences(ctx, docNodes,builder, pub); err != nil {
		return nil, fmt.Errorf("error resolving image references: %v", err)
	}

	buf := &bytes.Buffer{}
	e := yaml.NewEncoder(buf)
	e.SetIndent(2)

	for _, doc := range docNodes {
		err := e.Encode(doc)
		if err != nil {
			return nil, fmt.Errorf("failed to encode output: %v", err)
		}
	}
	e.Close()

	return buf.Bytes(), nil

}
