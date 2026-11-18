//go:build !windows

package downloads

import "os"

func publishFile(root *os.Root, from, to string) error {
	return root.Link(from, to)
}
