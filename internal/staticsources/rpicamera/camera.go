//go:build (linux && arm) || (linux && arm64)

package rpicamera

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/bluenviron/mediacommon/pkg/codecs/h264"
)

type camera struct {
	Params params
	OnData func(time.Duration, [][]byte)

	cmd     *exec.Cmd
	pipeOut *pipe
	pipeIn  *pipe

	waitDone   chan error
	readerDone chan error
}

func (c *camera) initialize() error {
	err := dumpComponent()
	if err != nil {
		return err
	}

	c.pipeOut, err = newPipe()
	if err != nil {
		freeComponent()
		return err
	}

	c.pipeIn, err = newPipe()
	if err != nil {
		c.pipeOut.close()
		freeComponent()
		return err
	}

	env := []string{
		"PIPE_CONF_FD=" + strconv.FormatInt(int64(c.pipeOut.readFD), 10),
		"PIPE_VIDEO_FD=" + strconv.FormatInt(int64(c.pipeIn.writeFD), 10),
		"LD_LIBRARY_PATH=" + dumpPath,
	}

	c.cmd = exec.Command(filepath.Join(dumpPath, executableName))
	c.cmd.Stdout = os.Stdout
	c.cmd.Stderr = os.Stderr
	c.cmd.Env = env
	c.cmd.Dir = dumpPath

	err = c.cmd.Start()
	if err != nil {
		c.pipeOut.close()
		c.pipeIn.close()
		freeComponent()
		return err
	}

	c.pipeOut.write(append([]byte{'c'}, c.Params.serialize()...))

	c.waitDone = make(chan error)
	go func() {
		c.waitDone <- c.cmd.Wait()
	}()

	c.readerDone = make(chan error)
	go func() {
		c.readerDone <- c.readReady()
	}()

	select {
	case err := <-c.waitDone:
		c.pipeOut.close()
		c.pipeIn.close()
		<-c.readerDone
		freeComponent()
		return fmt.Errorf("process exited unexpectedly: %v", err)

	case err := <-c.readerDone:
		if err != nil {
			c.pipeOut.write([]byte{'e'})
			<-c.waitDone
			c.pipeOut.close()
			c.pipeIn.close()
			freeComponent()
			return err
		}
	}

	c.readerDone = make(chan error)
	go func() {
		c.readerDone <- c.readData()
		close(c.readerDone)
	}()

	return nil
}

func (c *camera) close() {
	c.pipeOut.write([]byte{'e'})
	<-c.waitDone
	c.pipeOut.close()
	c.pipeIn.close()
	<-c.readerDone
	freeComponent()
}

func (c *camera) reloadParams(params params) {
	c.pipeOut.write(append([]byte{'c'}, params.serialize()...))
}

func (c *camera) readReady() error {
	buf, err := c.pipeIn.read()
	if err != nil {
		return err
	}

	switch buf[0] {
	case 'e':
		return fmt.Errorf(string(buf[1:]))

	case 'r':
		return nil

	default:
		return fmt.Errorf("unexpected output from video pipe: '0x%.2x'", buf[0])
	}
}

func (c *camera) readData() error {
	for {
		buf, err := c.pipeIn.read()
		if err != nil {
			return err
		}

		switch buf[0] {
		case 'e':
			return fmt.Errorf(string(buf[1:]))

		case 'b':
			tmp := uint64(buf[8])<<56 | uint64(buf[7])<<48 | uint64(buf[6])<<40 | uint64(buf[5])<<32 |
				uint64(buf[4])<<24 | uint64(buf[3])<<16 | uint64(buf[2])<<8 | uint64(buf[1])
			dts := time.Duration(tmp) * time.Microsecond

			nalus, err := h264.AnnexBUnmarshal(buf[9:])
			if err != nil {
				return err
			}

			c.OnData(dts, nalus)

		default:
			return fmt.Errorf("unexpected output from pipe (%c)", buf[0])
		}
	}
}

func (c *camera) error() chan error {
	return c.readerDone
}
