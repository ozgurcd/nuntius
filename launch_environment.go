package nuntius

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Ferry inherited environment through a private FIFO, never a disk snapshot.
// The writer is a bounded goroutine in the dispatching server, not a helper.
func environmentPipe(dir string, env []string) (func(), error) {
	b, err := json.Marshal(env)
	if err != nil || len(b) > 65536 {
		return nil, errors.New("launch environment too large")
	}
	data := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(data, uint32(len(b)))
	copy(data[4:], b)
	path := filepath.Join(dir, "environment.pipe")
	if err = syscall.Mkfifo(path, 0600); err != nil {
		return nil, errors.New("cannot create launch environment pipe")
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		fd := -1
		defer func() {
			if fd >= 0 {
				_ = syscall.Close(fd)
			}
		}()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			select {
			case <-stop:
				return
			default:
			}
			if fd < 0 {
				fd, err = syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
				if err != nil && err != syscall.ENXIO {
					return
				}
			}
			if fd >= 0 {
				n, e := syscall.Write(fd, data)
				if e != nil && e != syscall.EAGAIN && e != syscall.EINTR {
					return
				}
				if n > 0 {
					data = data[n:]
				}
				if len(data) == 0 {
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return func() { close(stop); <-done; _ = os.Remove(path) }, nil
}

func readLaunchEnvironment(dir string) ([]string, error) {
	fd, err := syscall.Open(filepath.Join(dir, "environment.pipe"), syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("launch environment unavailable")
	}
	f := os.NewFile(uintptr(fd), "launch environment pipe")
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		return nil, errors.New("unsafe launch environment pipe")
	}
	data, buf := make([]byte, 0, 4096), make([]byte, 4096)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		n, err := syscall.Read(fd, buf)
		if err != nil && err != syscall.EAGAIN && err != syscall.EINTR {
			return nil, errors.New("launch environment read failed")
		}
		if n > 0 {
			data = append(data, buf[:n]...)
		}
		if len(data) > 65540 {
			return nil, errors.New("launch environment too large")
		}
		if len(data) >= 4 {
			size := int(binary.BigEndian.Uint32(data[:4]))
			if size > 65536 {
				return nil, errors.New("launch environment too large")
			}
			if len(data) == 4+size {
				var env []string
				if json.Unmarshal(data[4:], &env) != nil {
					return nil, errors.New("invalid launch environment")
				}
				return env, nil
			}
			if len(data) > 4+size {
				return nil, errors.New("invalid launch environment size")
			}
		}
		time.Sleep(time.Millisecond)
	}
	return nil, errors.New("launch environment timed out")
}
