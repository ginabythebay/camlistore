// The camget tool fetches blobs, files, and directories.
//
// Examples
//
// Writes to stdout by default:
//
//   camget <blobref>                 // dump raw blob
//   camget -contents <file-blobref>  // dump file contents
//
// Like curl, lets you set output file/directory with -o:
//
//   camget -o <dir> <blobref>
//     (if <dir> exists and is directory, <blobref> must be a directory;
//      use -f to overwrite any files)
//
//   camget -o <filename> <file-blobref>
//
// TODO(bradfitz): camget isn't very fleshed out. In general, using 'cammount' to just
// mount a tree is an easier way to get files back.
package main

/*
Copyright 2011 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"camlistore.org/pkg/blobref"
	"camlistore.org/pkg/buildinfo"
	"camlistore.org/pkg/cacher"
	"camlistore.org/pkg/client"
	"camlistore.org/pkg/httputil"
	"camlistore.org/pkg/index"
	"camlistore.org/pkg/schema"
)

var (
	flagVersion  = flag.Bool("version", false, "show version")
	flagVerbose  = flag.Bool("verbose", false, "be verbose")
	flagHTTP     = flag.Bool("verbose_http", false, "show HTTP request summaries")
	flagCheck    = flag.Bool("check", false, "just check for the existence of listed blobs; returning 0 if all are present")
	flagOutput   = flag.String("o", "-", "Output file/directory to create.  Use -f to overwrite.")
	flagGraph    = flag.Bool("graph", false, "Output a graphviz directed graph .dot file of the provided root schema blob, to be rendered with 'dot -Tsvg -o graph.svg graph.dot'")
	flagContents = flag.Bool("contents", false, "If true and the target blobref is a 'bytes' or 'file' schema blob, the contents of that file are output instead.")
	flagShared   = flag.String("shared", "", "If non-empty, the URL of a \"share\" blob. The URL will be used as the root of future fetches. Only \"haveref\" shares are currently supported.")
)

func main() {
	client.AddFlags()
	flag.Parse()

	if *flagVersion {
		fmt.Fprintf(os.Stderr, "camget version: %s\n", buildinfo.Version())
		return
	}

	if *flagGraph && flag.NArg() != 1 {
		log.Fatalf("The --graph option requires exactly one parameter.")
	}

	var cl *client.Client
	var items []*blobref.BlobRef

	if *flagShared != "" {
		if client.ExplicitServer() != "" {
			log.Fatal("Can't use --shared with an explicit blobserver; blobserver is implicit from the --shared URL.")
		}
		if flag.NArg() != 0 {
			log.Fatal("No arguments permitted when using --shared")
		}
		cl1, target, err := client.NewFromShareRoot(*flagShared)
		if err != nil {
			log.Fatal(err)
		}
		cl = cl1
		items = append(items, target)
	} else {
		cl = client.NewOrFail()
		for n := 0; n < flag.NArg(); n++ {
			arg := flag.Arg(n)
			br := blobref.Parse(arg)
			if br == nil {
				log.Fatalf("Failed to parse argument %q as a blobref.", arg)
			}
			items = append(items, br)
		}
	}

	httpStats := &httputil.StatsTransport{
		VerboseLog: *flagHTTP,
	}
	if *flagHTTP {
		httpStats.Transport = &http.Transport{
			Dial: func(net_, addr string) (net.Conn, error) {
				log.Printf("Dialing %s", addr)
				return net.Dial(net_, addr)
			},
		}
	}
	cl.SetHTTPClient(&http.Client{Transport: httpStats})

	diskCacheFetcher, err := cacher.NewDiskCache(cl)
	if err != nil {
		log.Fatalf("Error setting up local disk cache: %v", err)
	}
	defer diskCacheFetcher.Clean()
	if *flagVerbose {
		log.Printf("Using temp blob cache directory %s", diskCacheFetcher.Root)
	}

	for _, br := range items {
		if *flagGraph {
			printGraph(diskCacheFetcher, br)
			return
		}
		if *flagCheck {
			// TODO: do HEAD requests checking if the blobs exists.
			log.Fatal("not implemented")
			return
		}
		if *flagOutput == "-" {
			var rc io.ReadCloser
			var err error
			if *flagContents {
				rc, err = schema.NewFileReader(diskCacheFetcher, br)
				if err == nil {
					rc.(*schema.FileReader).LoadAllChunks()
				}
			} else {
				rc, err = fetch(diskCacheFetcher, br)
			}
			if err != nil {
				log.Fatal(err)
			}
			defer rc.Close()
			if _, err := io.Copy(os.Stdout, rc); err != nil {
				log.Fatalf("Failed reading %q: %v", br, err)
			}
		} else {
			if err := smartFetch(diskCacheFetcher, *flagOutput, br); err != nil {
				log.Fatal(err)
			}
		}
	}

	if *flagVerbose {
		log.Printf("HTTP requests: %d\n", httpStats.Requests())
	}
}

func fetch(src blobref.StreamingFetcher, br *blobref.BlobRef) (r io.ReadCloser, err error) {
	if *flagVerbose {
		log.Printf("Fetching %s", br.String())
	}
	r, _, err = src.FetchStreaming(br)
	if err != nil {
		return nil, fmt.Errorf("Failed to fetch %s: %s", br, err)
	}
	return r, err
}

// A little less than the sniffer will take, so we don't truncate.
const sniffSize = 900 * 1024

// smartFetch the things that blobs point to, not just blobs.
func smartFetch(src blobref.StreamingFetcher, targ string, br *blobref.BlobRef) error {
	rc, err := fetch(src, br)
	if err != nil {
		return err
	}
	defer rc.Close()

	sniffer := index.NewBlobSniffer(br)
	_, err = io.CopyN(sniffer, rc, sniffSize)
	if err != nil && err != io.EOF {
		return err
	}

	sniffer.Parse()
	blob, ok := sniffer.SchemaBlob()

	if !ok {
		if *flagVerbose {
			log.Printf("Fetching opaque data %v into %q", br, targ)
		}

		// opaque data - put it in a file
		f, err := os.Create(targ)
		if err != nil {
			return fmt.Errorf("opaque: %v", err)
		}
		defer f.Close()
		body, _ := sniffer.Body()
		r := io.MultiReader(bytes.NewReader(body), rc)
		_, err = io.Copy(f, r)
		return err
	}

	switch blob.Type() {
	case "directory":
		dir := filepath.Join(targ, blob.FileName())
		if *flagVerbose {
			log.Printf("Fetching directory %v into %s", br, dir)
		}
		if err := os.MkdirAll(dir, blob.FileMode()); err != nil {
			return err
		}
		if err := setFileMeta(dir, blob); err != nil {
			log.Print(err)
		}
		entries := blob.DirectoryEntries()
		if entries == nil {
			return fmt.Errorf("bad entries blobref in dir %v", blob.BlobRef())
		}
		return smartFetch(src, dir, entries)
	case "static-set":
		if *flagVerbose {
			log.Printf("Fetching directory entries %v into %s", br, targ)
		}

		// directory entries
		const numWorkers = 10
		type work struct {
			br   *blobref.BlobRef
			errc chan<- error
		}
		members := blob.StaticSetMembers()
		workc := make(chan work, len(members))
		defer close(workc)
		for i := 0; i < numWorkers; i++ {
			go func() {
				for wi := range workc {
					wi.errc <- smartFetch(src, targ, wi.br)
				}
			}()
		}
		var errcs []<-chan error
		for _, mref := range members {
			errc := make(chan error, 1)
			errcs = append(errcs, errc)
			workc <- work{mref, errc}
		}
		for _, errc := range errcs {
			if err := <-errc; err != nil {
				return err
			}
		}
		return nil
	case "file":
		seekFetcher := blobref.SeekerFromStreamingFetcher(src)
		fr, err := schema.NewFileReader(seekFetcher, br)
		if err != nil {
			return fmt.Errorf("NewFileReader: %v", err)
		}
		fr.LoadAllChunks()
		defer fr.Close()

		name := filepath.Join(targ, blob.FileName())

		if fi, err := os.Stat(name); err == nil && fi.Size() == fi.Size() {
			if *flagVerbose {
				log.Printf("Skipping %s; already exists.", name)
				return nil
			}
		}

		if *flagVerbose {
			log.Printf("Writing %s to %s ...", br, name)
		}

		f, err := os.Create(name)
		if err != nil {
			return fmt.Errorf("file type: %v", err)
		}
		defer f.Close()
		if _, err := io.Copy(f, fr); err != nil {
			return fmt.Errorf("Copying %s to %s: %v", br, name, err)
		}
		if err := setFileMeta(name, blob); err != nil {
			log.Print(err)
		}
		return nil
	default:
		return errors.New("unknown blob type: " + blob.Type())
	}
	panic("unreachable")
}

func setFileMeta(name string, blob *schema.Blob) error {
	err1 := os.Chmod(name, blob.FileMode())
	var err2 error
	if mt := blob.ModTime(); !mt.IsZero() {
		err2 = os.Chtimes(name, mt, mt)
	}
	// TODO: we previously did os.Chown here, but it's rarely wanted,
	// then the schema.Blob refactor broke it, so it's gone.
	// Add it back later once we care?
	for _, err := range []error{err1, err2} {
		if err != nil {
			return err
		}
	}
	return nil
}
