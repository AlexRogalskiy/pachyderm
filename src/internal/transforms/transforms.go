// package transforms contains PPS Pipeline Transform implementations
package transforms

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// DataMapper maps one stream to another or errors
type DataMapper = func(r io.Reader, w io.Writer) error

// IdentityDM is the DataMapper which maps data to itself
func IdentityDM(r io.Reader, w io.Writer) error {
	_, err := io.Copy(w, r)
	return err
}

// PathMapper is a function that maps one path to another
type PathMapper = func(string) string

// IdentityPM is the PathMapper which maps a path to itself
func IdentityPM(x string) string {
	return x
}

// bijectiveMap walks files in inputDir and applies a mapping to both the path
// and the file content and writes the result to the corresponding path in outputDir
//
// To leave paths unchanged use IdentityPM for pm
// To leave file content unchanged use IdentityDM for dm
func bijectiveMap(inputDir, outputDir string, pm PathMapper, dm DataMapper) error {
	return filepath.WalkDir(inputDir, func(inputPath string, dirEnt fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if dirEnt.IsDir() {
			return nil
		}
		inputRelPath, err := filepath.Rel(inputDir, inputPath)
		if err != nil {
			return err
		}
		outputPath := filepath.Join(outputDir, pm(inputRelPath))
		inputFile, err := os.OpenFile(inputPath, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		defer inputFile.Close()
		outputFile, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE, 0755)
		if err != nil {
			return err
		}
		defer outputFile.Close()
		if err := dm(inputFile, outputFile); err != nil {
			return err
		}
		return outputFile.Close()
	})
}
