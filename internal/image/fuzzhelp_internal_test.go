package image

import (
	"archive/tar"
	"io"
)

// writeSingleEntryTar renders one regular-file entry as a tar archive. It lives
// beside the fuzz targets because it is the only thing that builds an archive
// inside the package's own test binary; the richer builder used by the external
// tests cannot be reached from here.
func writeSingleEntryTar(w io.Writer, name, body string) error {
	archive := tar.NewWriter(w)

	header := &tar.Header{
		Name:     name,
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     int64(len(body)),
	}
	if err := archive.WriteHeader(header); err != nil {
		return err
	}
	if _, err := archive.Write([]byte(body)); err != nil {
		return err
	}

	return archive.Close()
}
