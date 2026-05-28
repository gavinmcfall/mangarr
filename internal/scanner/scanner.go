package scanner

import (
	"archive/zip"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gavinmcfall/mangarr/internal/model"
)

type comicInfo struct {
	Series string `xml:"Series"`
}

// Scan walks one download root. A "series" is any directory that directly
// contains .cbz files. Title comes from the first CBZ's ComicInfo.xml <Series>,
// falling back to the directory's base name.
func Scan(root, source string) ([]model.Series, error) {
	bySeriesDir := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries; don't abort the whole scan
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".cbz") {
			dir := filepath.Dir(path)
			bySeriesDir[dir] = append(bySeriesDir[dir], path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	var out []model.Series
	for dir, cbzs := range bySeriesDir {
		if len(cbzs) == 0 {
			continue // defensive: never index into an empty slice
		}
		sort.Strings(cbzs) // deterministic title source regardless of WalkDir order
		title := seriesTitleFromCBZ(cbzs[0])
		if title == "" {
			title = filepath.Base(dir)
		}
		out = append(out, model.Series{
			Title:        title,
			SourcePath:   dir,
			Source:       source,
			Status:       model.StatusPending,
			ChapterCount: len(cbzs),
		})
	}
	return out, nil
}

func seriesTitleFromCBZ(path string) string {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return ""
	}
	defer zr.Close()
	for _, f := range zr.File {
		if strings.EqualFold(f.Name, "ComicInfo.xml") {
			rc, err := f.Open()
			if err != nil {
				return ""
			}
			defer rc.Close()
			data, err := io.ReadAll(rc)
			if err != nil {
				return ""
			}
			var ci comicInfo
			if xml.Unmarshal(data, &ci) == nil {
				return strings.TrimSpace(ci.Series)
			}
		}
	}
	return ""
}
