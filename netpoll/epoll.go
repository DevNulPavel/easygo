// +build linux

package netpoll

import (
	"sync"

	"golang.org/x/sys/unix"
)

// EpollEvent represents epoll events configuration bit mask.
type EpollEvent uint32

// EpollEvents that are mapped to epoll_event.events possible values.
const (
	EPOLLIN      = unix.EPOLLIN
	EPOLLOUT     = unix.EPOLLOUT
	EPOLLRDHUP   = unix.EPOLLRDHUP
	EPOLLPRI     = unix.EPOLLPRI
	EPOLLERR     = unix.EPOLLERR
	EPOLLHUP     = unix.EPOLLHUP
	EPOLLET      = unix.EPOLLET
	EPOLLONESHOT = unix.EPOLLONESHOT

	// _EPOLLCLOSED is a special EpollEvent value the receipt of which means
	// that the epoll instance is closed.
	_EPOLLCLOSED = 0x20
)

// String returns a string representation of EpollEvent.
func (evt EpollEvent) String() (str string) {
	name := func(event EpollEvent, name string) {
		if evt&event == 0 {
			return
		}
		if str != "" {
			str += "|"
		}
		str += name
	}

	name(EPOLLIN, "EPOLLIN")
	name(EPOLLOUT, "EPOLLOUT")
	name(EPOLLRDHUP, "EPOLLRDHUP")
	name(EPOLLPRI, "EPOLLPRI")
	name(EPOLLERR, "EPOLLERR")
	name(EPOLLHUP, "EPOLLHUP")
	name(EPOLLET, "EPOLLET")
	name(EPOLLONESHOT, "EPOLLONESHOT")
	name(_EPOLLCLOSED, "_EPOLLCLOSED")

	return
}

// Epoll represents single epoll instance.
type Epoll struct {
	mu sync.RWMutex

	fd       int
	eventFd  int
	closed   bool
	waitDone chan struct{}

	callbacks map[int]func(EpollEvent)
}

// EpollConfig contains options for Epoll instance configuration.
type EpollConfig struct {
	// OnWaitError will be called from goroutine, waiting for events.
	OnWaitError func(error)
}

func (c *EpollConfig) withDefaults() (config EpollConfig) {
	if c != nil {
		config = *c
	}
	if config.OnWaitError == nil {
		config.OnWaitError = defaultOnWaitError
	}
	return config
}

// EpollCreate creates new epoll instance.
// It starts the wait loop in separate goroutine.
func EpollCreate(c *EpollConfig) (*Epoll, error) {
	config := c.withDefaults()

	fd, err := unix.EpollCreate1(0)
	if err != nil {
		return nil, err
	}

	r0, _, errno := unix.Syscall(unix.SYS_EVENTFD2, 0, 0, 0)
	if errno != 0 {
		return nil, errno
	}
	eventFd := int(r0)

	// Set finalizer for write end of socket pair to avoid data races when
	// closing Epoll instance and EBADF errors on writing ctl bytes from callers.
	err = unix.EpollCtl(fd, unix.EPOLL_CTL_ADD, eventFd, &unix.EpollEvent{
		Events: unix.EPOLLIN,
		Fd:     int32(eventFd),
	})
	if err != nil {
		unix.Close(fd)
		unix.Close(eventFd)
		return nil, err
	}

	ep := &Epoll{
		fd:        fd,
		eventFd:   eventFd,
		callbacks: make(map[int]func(EpollEvent)),
		waitDone:  make(chan struct{}),
	}

	// Запускаем горутину, которая отслеживает изменения
	go ep.wait(config.OnWaitError)

	return ep, nil
}

// closeBytes used for writing to eventfd.
var closeBytes = []byte{1, 0, 0, 0, 0, 0, 0, 0}

// Close stops wait loop and closes all underlying resources.
func (ep *Epoll) Close() (err error) {
	ep.mu.Lock()
	{
		if ep.closed {
			ep.mu.Unlock()
			return ErrClosed
		}
		ep.closed = true

		if _, err = unix.Write(ep.eventFd, closeBytes); err != nil {
			ep.mu.Unlock()
			return
		}
	}
	ep.mu.Unlock()

	<-ep.waitDone

	if err = unix.Close(ep.eventFd); err != nil {
		return
	}

	ep.mu.Lock()
	// Set callbacks to nil preventing long mu.Lock() hold.
	// This could increase the speed of retreiving ErrClosed in other calls to
	// current epoll instance.
	// Setting callbacks to nil is safe here because no one should read after
	// closed flag is true.
	callbacks := ep.callbacks
	ep.callbacks = nil
	ep.mu.Unlock()

	for _, cb := range callbacks {
		if cb != nil {
			cb(_EPOLLCLOSED)
		}
	}

	return
}

// Add добавляет файловые дескрипторы для отслеживания с помощью epoll
// Важно! _EPOLLCLOSED вызывается для каждого коллбека когда epoll закрывается
func (ep *Epoll) Add(fd int, events EpollEvent, cb func(EpollEvent)) (err error) {
	// Создаем ивент
	ev := &unix.EpollEvent{
		Events: uint32(events),
		Fd:     int32(fd),
	}

	ep.mu.Lock()
	defer ep.mu.Unlock()

	if ep.closed {
		return ErrClosed
	}

	// Проверяем, не сохранен ли уже коллбек для данного файлового дескриптора
	if _, has := ep.callbacks[fd]; has {
		return ErrRegistered
	}
	// Сохраняем коллбек
	ep.callbacks[fd] = cb

	// Подключаем файловый дескриптор к отслеживанию с помощью epoll
	return unix.EpollCtl(ep.fd, unix.EPOLL_CTL_ADD, fd, ev)
}

// Del удаляет файловый дескриптор из отслеживания с помощью epoll
func (ep *Epoll) Del(fd int) (err error) {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	if ep.closed {
		return ErrClosed
	}
	if _, ok := ep.callbacks[fd]; !ok {
		return ErrNotRegistered
	}

	// Удаляем коллбек
	delete(ep.callbacks, fd)

	// Удаляем файловый дескриптор
	return unix.EpollCtl(ep.fd, unix.EPOLL_CTL_DEL, fd, nil)
}

// Mod изменяет настройки для отслеживания файлового дескриптора
func (ep *Epoll) Mod(fd int, events EpollEvent) (err error) {
	// Создаем ивент
	ev := &unix.EpollEvent{
		Events: uint32(events),
		Fd:     int32(fd),
	}

	ep.mu.RLock()
	defer ep.mu.RUnlock()

	if ep.closed {
		return ErrClosed
	}
	if _, ok := ep.callbacks[fd]; !ok {
		return ErrNotRegistered
	}

	// Изменяем настройки
	return unix.EpollCtl(ep.fd, unix.EPOLL_CTL_MOD, fd, ev)
}

const (
	maxWaitEventsBegin = 1024
	maxWaitEventsStop  = 32768
)

func (ep *Epoll) wait(onError func(error)) {
	// Отложенная функция, которая автоматически закрывает файловый дескриптор epoll и канал завершения работы
	defer func() {
		if err := unix.Close(ep.fd); err != nil {
			onError(err)
		}
		close(ep.waitDone)
	}()

	// Создаем начальные массивы для событий и коллбеков для цикла
	events := make([]unix.EpollEvent, maxWaitEventsBegin)
	callbacks := make([]func(EpollEvent), 0, maxWaitEventsBegin)

	for {
		// Ждем от системы когда что-то поменяется в отслеживаемых файловых дескрипторах
		n, err := unix.EpollWait(ep.fd, events, -1)
		if err != nil {
			if temporaryErr(err) {
				continue
			}
			onError(err)
			return
		}

		// Обновляем размер слайса коллбеков
		callbacks = callbacks[:n]

		// Получаем коллбеки для обновленных файловых дескрипторов
		ep.mu.RLock()
		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)
			if fd == ep.eventFd { // signal to close
				ep.mu.RUnlock()
				return
			}
			callbacks[i] = ep.callbacks[fd]
		}
		ep.mu.RUnlock()

		// Вызываем коллбек для каждого обновленного файлового дескриптора
		for i := 0; i < n; i++ {
			if cb := callbacks[i]; cb != nil {
				cb(EpollEvent(events[i].Events))
				callbacks[i] = nil
			}
		}

		// Расширяем при необходимости массивый элементов если не слезало
		if n == len(events) && n*2 <= maxWaitEventsStop {
			events = make([]unix.EpollEvent, n*2)
			callbacks = make([]func(EpollEvent), 0, n*2)
		}
	}
}
