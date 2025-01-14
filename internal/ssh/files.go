// Copyright © 2019 Alvaro Saurin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ssh

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	defMaxPathLength = 4096

	defTemporaryFilenamePrefix = "tmpfile"

	defTemporaryFilenameExt = "tmp"

	defaultRemoteTmp = "/tmp"

	markStart = "-- START --"

	markEnd = "-- END --"
)

// LocalFileExists reports whether the named file or directory exists.
func LocalFileExists(name string) bool {
	if len(name) > defMaxPathLength {
		return false
	}
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

func randBytes(length int) (string, error) {
	b := make([]byte, length)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// randomPath gets a random Path
func randomPath(prefix, extension string) (string, error) {
	r, err := randBytes(3)
	if err != nil {
		return "", err
	}
	if len(prefix) == 0 || len(extension) == 0 {
		return "", fmt.Errorf("can not use empty Prefix or extension")
	}
	return fmt.Sprintf("%s/%s-%s.%s", defaultRemoteTmp, prefix, r, extension), nil
}

// GetTempFilename returns a temporary filename (but it does not create it)
func GetTempFilename() (string, error) {
	return randomPath(defTemporaryFilenamePrefix, defTemporaryFilenameExt)
}

// IsTempFilename returns true if it is a temporary filename
func IsTempFilename(filename string) bool {
	base := path.Base(filename)
	if !strings.HasPrefix(base, defTemporaryFilenamePrefix) {
		return false
	}
	if !strings.HasSuffix(base, defTemporaryFilenameExt) {
		return false
	}
	return true
}

// doRealUploadFile uploads a file to a remote path
func doRealUploadFile(contents []byte, dst string) Action {
	if len(dst) == 0 {
		return DoAbort("empty destination for upload")
	}

	dstDir := filepath.Dir(dst)

	actions := ActionList{
		DoMkdirOnce(dstDir),
		DoMessageDebug(fmt.Sprintf("Making sure '%s' does not exist", dst)),
		DoDeleteFile(dst),
		ActionFunc(func(ctx context.Context) Action {
			if len(contents) == 0 {
				return ActionError(fmt.Sprintf("internal error: empty file to upload to %q", dst))
			}

			c := bytes.NewReader(contents)
			comm := GetCommFromContext(ctx)

			Debug("Doing the real upload to %s:\n%s\n", dst, contents)
			if err := comm.Upload(dst, c); err != nil {
				Debug("ERROR: upload failed: %s", err)
				return ActionError(err.Error())
			}

			return nil
		}),
	}

	return actions
}

// DoUploadBytesToFile uploads a file to a remote path, using a temporary file in /tmp
// and then moving it to the final destination with `sudo`.
// It is important to use a temporary file as uploads are performed as a regular
// user, while the `mv` is done with `sudo`
func DoUploadBytesToFile(contents []byte, dst string) Action {
	if len(dst) == 0 {
		return ActionError(fmt.Sprintf("internal error: empty remote path in DoUploadBytesToFile()"))
	}

	// do not create temporary files for files that are already in the remote temporary directory
	if IsTempFilename(dst) {
		return doRealUploadFile(contents, dst)
	}

	// for regular files, upload to a temp file and then move the temp file to the final destination
	// (uploading directly to destination could need root permissions, while we can "mv" with "sudo")
	dstTmpPath, err := GetTempFilename()
	if err != nil {
		return ActionError(fmt.Sprintf("Could not create temporary file: %s", err))
	}

	return DoWithCleanup(ActionList{
		DoMessageInfo(fmt.Sprintf("Uploading to %q", dst)),
		DoMessageDebug(fmt.Sprintf("Uploading to temporary file %q", dstTmpPath)),
		doRealUploadFile(contents, dstTmpPath),
		DoMessageDebug(fmt.Sprintf("... and moving to final destination %s", dst)),
		DoMoveFile(dstTmpPath, dst),
	}, ActionList{
		DoTry(DoDeleteFile(dstTmpPath)),
	})
}

// DoUploadFileToFile uploads a local file to a remote file (using a temporary file)
func DoUploadFileToFile(local string, remote string) Action {
	if local == "" {
		return ActionError("empty local file name to upload")
	}
	if remote == "" {
		return ActionError("empty remote file name to upload")
	}

	return ActionFunc(func(context.Context) Action {
		// note: we must do the "Open" inside the ActionFunc, as we must delay the operation
		// just in case the file does not exists yet
		f, err := os.Open(local)
		if err != nil {
			return ActionError(fmt.Sprintf("could not open local file %q for uploading to %q: %s", local, remote, err))
		}

		b, err := ioutil.ReadAll(f)
		if err != nil {
			return ActionError(fmt.Sprintf("could not read local file %q for uploading to %q: %s", local, remote, err))
		}

		return DoUploadBytesToFile(b, remote)
	})
}

// DoDownloadFileToWriter downloads a file to a writer
func DoDownloadFileToWriter(remote string, contents io.WriteCloser) Action {
	if remote == "" {
		return ActionError("empty remote file name to download")
	}

	// Terraform does not provide a mechanism for copying file from a remote host
	// to the local machine
	// so we must run a remote command that dumps the file Contents to stdout
	// hopefully it will be terminal-friendly
	// otherwise, we could use `cat <FILE> | base64 -`
	command := fmt.Sprintf("sh -c \"echo '%s' && cat '%s' && echo '%s'\"", markStart, remote, markEnd)

	insideBlock := false
	extraOutput := ""
	var err error

	return DoWithCleanup(ActionList{
		DoMessageDebug(fmt.Sprintf("Dumping remote file %q", remote)),
		DoSendingExecOutputToFunc(
			DoExec(command),
			func(s string) {
				if strings.Contains(s, markStart) {
					insideBlock = true
					return
				}
				if strings.Contains(s, markEnd) {
					insideBlock = false
					return
				}

				if insideBlock {
					if _, err = contents.Write([]byte(s)); err != nil {
						return
					}

					if _, err = contents.Write([]byte{'\n'}); err != nil {
						return
					}
				} else {
					extraOutput += s
				}
			}),
	}, ActionList{
		DoMessage(extraOutput),
		ActionFunc(func(context.Context) Action {
			// close the file handler
			_ = contents.Close()
			return nil
		}),
	})
}

// DoWriteLocalFile writes some string in a local file
func DoWriteLocalFile(path string, contents string) Action {
	if path == "" {
		return ActionError("empty local file name to create")
	}
	return ActionFunc(func(context.Context) Action {
		localFile, err := os.Create(path)
		if err != nil {
			return ActionError(fmt.Sprintf("cannot create %q: %s", path, err.Error()))
		}
		if _, err := localFile.WriteString(contents); err != nil {
			return ActionError(fmt.Sprintf("cannot write %q: %s", path, err.Error()))
		}
		return nil
	})
}

// DoDeleteFile removes a file
func DoDeleteFile(path string) Action {
	if path == "" {
		return ActionError("empty remote file name to remove")
	}
	return ActionList{
		DoExec(fmt.Sprintf("rm -f %q", path)),
		DoRemoveFromCache(CacheRemoteFileExistsPrefix + "-" + path),
	}
}

// DoDeleteLocalFile removes a local file
func DoDeleteLocalFile(path string) Action {
	if path == "" {
		return ActionError("empty local file name to remove")
	}
	return DoLocalExec(fmt.Sprintf("rm -f %q", path))
}

// DoMoveFile moves a file
func DoMoveFile(src, dst string) Action {
	dstDir := filepath.Dir(dst)
	return DoExec(fmt.Sprintf("mkdir -p %q && mv -f %q %q", dstDir, src, dst))
}

// DoMoveLocalFile moves a local file
func DoMoveLocalFile(src, dst string) Action {
	dstDir := filepath.Dir(dst)
	return ActionList{
		DoLocalExec("mkdir", dstDir),
		DoLocalExec("mv", "-f", src, dst),
	}
}

// DoDownloadFile downloads a remote file to a local file
func DoDownloadFile(remote, local string) Action {
	return ActionFunc(func(context.Context) Action {
		localFile, err := os.Create(local)
		if err != nil {
			return ActionError(err.Error())
		}
		return ActionList{
			DoMessageInfo(fmt.Sprintf("Downloading remote file %q -> %q", remote, local)),
			DoDownloadFileToWriter(remote, localFile),
		}
	})
}

//
// leftovers
//

// DoAddLeftover adds a leftover file
func DoAddLeftover(path string) Action {
	return ActionFunc(func(ctx context.Context) Action {
		sshc := getSSHContext(ctx)
		sshc.leftovers = append(sshc.leftovers, path)
		return nil
	})
}

// DoCleanupLeftovers removes all the leftovers files
func DoCleanupLeftovers() Action {
	return ActionFunc(func(ctx context.Context) Action {
		sshc := getSSHContext(ctx)
		if len(sshc.leftovers) == 0 {
			return nil
		}

		actions := ActionList{
			DoMessageInfo("Removing leftovers..."),
		}
		for _, l := range sshc.leftovers {
			actions = append(actions, DoDeleteFile(l))
		}
		return actions
	})
}

///////////////////////////////////////////////////////////////////////////////////////
// checks
///////////////////////////////////////////////////////////////////////////////////////

// CheckFileExists checks that a remote file exists
func CheckFileExists(path string) CheckerFunc {
	return CheckExec(fmt.Sprintf("[ -f %s ]", path))
}

// CheckFileExistsOnce checks that a remote file exists (but only once)
func CheckFileExistsOnce(path string) CheckerFunc {
	return CheckOnce(
		CacheRemoteFileExistsPrefix+"-"+path,
		CheckFileExists(path))
}

// CheckFileAbsent checks that a remote file does not exists
func CheckFileAbsent(path string) CheckerFunc {
	return CheckNot(CheckFileExists(path))
}

// CheckLocalFileExists checks that a local file exists
// If the input file is empty, it returns false.
func CheckLocalFileExists(path string) CheckerFunc {
	return CheckerFunc(func(context.Context) (bool, error) {
		if path == "" {
			return false, nil
		}
		if _, err := os.Stat(path); err == nil {
			return true, nil
		}
		return false, nil
	})
}
