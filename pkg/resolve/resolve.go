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

package resolve

import (
	"fmt"
	"strings"
	"sync"

	"github.com/dprotaso/go-yit"
	"github.com/google/ko/pkg/build"
	"github.com/google/ko/pkg/publish"
	"golang.org/x/sync/errgroup"
	"gopkg.in/yaml.v3"
)

const koPrefix = "ko://"

// ImageReferences resolves supported references to images within the input yaml
// to published image digests.
//
// If a reference can be built and pushed, its yaml.Node will be mutated.
func ImageReferences(docs []*yaml.Node, strict bool, builder build.Interface, publisher publish.Interface) error {
	// First, walk the input objects and collect a list of supported references
	refs := make(map[string][]*yaml.Node)

	for _, doc := range docs {
		it := refsFromDoc(doc, strict)

		for node, ok := it(); ok; node, ok = it() {
			ref := strings.TrimSpace(node.Value)
			tref := strings.TrimPrefix(ref, koPrefix)

			if builder.IsSupportedReference(tref) {
				refs[tref] = append(refs[tref], node)
			} else if strict {
				return fmt.Errorf("found strict reference but %s is not a valid import path", ref)
			}
		}
	}

	// Next, perform parallel builds for each of the supported references.
	var sm sync.Map
	var errg errgroup.Group
	for ref := range refs {
		ref := ref
		errg.Go(func() error {
			img, err := builder.Build(ref)
			if err != nil {
				return err
			}
			digest, err := publisher.Publish(img, ref)
			if err != nil {
				return err
			}
			sm.Store(ref, digest.String())
			return nil
		})
	}
	if err := errg.Wait(); err != nil {
		return nil, err
	}

	// Walk the tags and update them with their digest.
	for ref, nodes := range refs {
		digest, ok := sm.Load(ref)

		if !ok {
			return fmt.Errorf("resolved reference to %q not found", ref)
		}

		for _, node := range nodes {
			node.Value = digest.(string)
		}
	}

	return nil
}

func refsFromDoc(doc *yaml.Node, strict bool) yit.Iterator {
	it := yit.FromNode(doc).
		RecurseNodes().
		Filter(yit.StringValue)

	if strict {
		return it.Filter(yit.WithPrefix(koPrefix))
	}

	return it
}
