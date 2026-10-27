// Normalize a Helm-produced chart archive for byte-for-byte comparison.
package main

import (
	"archive/tar"
	"cmp"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"time"
)

type entry struct {
	name string
	data []byte
}

func main() {
	if len(os.Args) != 4 {
		panic("usage: normalize-chart input.tgz output.tgz source-date-epoch")
	}
	epoch, err := strconv.ParseInt(os.Args[3], 10, 64)
	check(err)
	timestamp := time.Unix(epoch, 0).UTC()
	in, err := os.Open(os.Args[1])
	check(err)
	defer func() { _ = in.Close() }()
	zr, err := gzip.NewReader(in)
	check(err)
	defer func() { _ = zr.Close() }()
	tr := tar.NewReader(zr)
	var files []entry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		check(err)
		if h.Typeflag != tar.TypeReg {
			panic(fmt.Sprintf("unexpected chart archive entry %q", h.Name))
		}
		data, err := io.ReadAll(tr)
		check(err)
		files = append(files, entry{h.Name, data})
	}
	slices.SortFunc(files, func(a, b entry) int { return cmp.Compare(a.name, b.name) })
	out, err := os.Create(os.Args[2])
	check(err)
	zw, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	check(err)
	zw.ModTime = time.Unix(0, 0)
	tw := tar.NewWriter(zw)
	for _, f := range files {
		h := &tar.Header{
			Name: f.name, Mode: 0644, Size: int64(len(f.data)), ModTime: timestamp,
			Uid: 0, Gid: 0, Format: tar.FormatPAX,
		}
		check(tw.WriteHeader(h))
		_, err = tw.Write(f.data)
		check(err)
	}
	check(tw.Close())
	check(zw.Close())
	check(out.Close())
	check(os.Chtimes(os.Args[2], timestamp, timestamp))
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
