//go:build !unix

package app

import "os"

func keepOwner(*os.File, os.FileInfo) error {
	return nil
}
