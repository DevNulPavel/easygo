/*
Пакет netpoll предоставляет портируемый интерфейс для сетевый событий ввода-вывода.

Данное API предназначено для мониторинга множества файловых дескрипторов, чтобы отследить,
что какие-то события ввода-вывода доступны на одном из них. Поддерживаются edge-triggered и level-triggered
интерфейсы.

Для получения дополнительной информации нужно смотреть описание API контретной операционной системы:
	- epoll on linux;
	- kqueue on bsd;

Функция Handle создает netpoll.Desc для дальнейшего использования в методах пулера:

	desc, err := netpoll.Handle(conn, netpoll.EventRead | netpoll.EventEdgeTriggered)
	if err != nil {
		// handle error
	}

Poller описывает платформозависимый интерфейс пулера:

	poller, err := netpoll.New(nil)
	if err != nil {
		// handle error
	}

	// Краткое создание дескриптора с режимом EventRead|EventEdgeTriggered.
	desc := netpoll.Must(netpoll.HandleRead(conn))

	// Установка коллбека на событие
	poller.Start(desc, func(ev netpoll.Event) {
		if (ev & netpoll.EventReadHup) != 0 {
			poller.Stop(desc)
			conn.Close()
			return
		}

		_, err := ioutil.ReadAll(conn)
		if err != nil {
			// handle error
		}
	})
*/
package netpoll

import (
	"fmt"
	"log"
)

var (
	// ErrNotFiler возвращается из Handle* фуункций для информирования,
	// что данное net.Conn не предоставляет доступ к данному дескриптору
	ErrNotFiler = fmt.Errorf("could not get file descriptor")

	// ErrClosed is returned by Poller methods to indicate that instance is
	// closed and operation could not be processed.
	ErrClosed = fmt.Errorf("poller instance is closed")

	// ErrRegistered is returned by Poller Start() method to indicate that
	// connection with the same underlying file descriptor was already
	// registered within the poller instance.
	ErrRegistered = fmt.Errorf("file descriptor is already registered in poller instance")

	// ErrNotRegistered is returned by Poller Stop() and Resume() methods to
	// indicate that connection with the same underlying file descriptor was
	// not registered before within the poller instance.
	ErrNotRegistered = fmt.Errorf("file descriptor was not registered before in poller instance")
)

// Event Описывает битовую маску конфигурации netpoll
type Event uint16

// Event значения, которые описывают типы событий, которые вызываемый хочет получать
const (
	EventRead  Event = 0x1
	EventWrite       = 0x2
)

// Event значения, которые описывают поведение Poller
const (
	EventOneShot       Event = 0x4
	EventEdgeTriggered       = 0x8
)

// Event значения, которые могут быть переданы в CallbackFn как дополнительная информация о событии
const (
	// EventHup говорит, что соединение было закрыто для записи или чтения (или вместе)
	// Usually (depending on operating system and its version) the EventReadHup or EventWriteHup are also set int Event value.
	EventHup Event = 0x10

	EventReadHup  = 0x20
	EventWriteHup = 0x40

	EventErr = 0x80

	// Значение информирует, что пулер был закрыт
	EventPollerClosed = 0x8000
)

// Строковое представление события
func (ev Event) String() (str string) {
	name := func(event Event, name string) {
		if (ev & event) == 0 {
			return
		}
		if str != "" {
			str += "|"
		}
		str += name
	}

	name(EventRead, "EventRead")
	name(EventWrite, "EventWrite")
	name(EventOneShot, "EventOneShot")
	name(EventEdgeTriggered, "EventEdgeTriggered")
	name(EventReadHup, "EventReadHup")
	name(EventWriteHup, "EventWriteHup")
	name(EventHup, "EventHup")
	name(EventErr, "EventErr")
	name(EventPollerClosed, "EventPollerClosed")

	return
}

// Poller интерфейс, который описывает базовые методы для всех платформ
type Poller interface {
	// Start добавляет к списку обзервером новый дескриптор и устанавливает функцию
	//
	// Помните, что если дескриптор сконфигурирован с режимом OneShot, пулер
	// то дескриптор будет удален после вызова события из пулера вместе с коллбеком.
	// Если нужно, чтобы можно было получать события снова - нужно вызвать Resume(desc).
	//
	// Однако вызов Resume() напрямую из коллбека приведет к дедлоку.
	//
	// Множественные вызовы с одним и тем же дескриптором приведут к непредвиденному поведению.
	Start(*Desc, CallbackFn) error

	// Stop удаляет дескриптор из списка отслеживания
	//
	// Помните, что данный вызов не вызывает desc.Close(). Это надо делать руками.
	Stop(*Desc) error

	// Resume включает снова дескриптор в список обработки
	//
	// Это полезно, когда дескриптор сконфигурирован с EventOneShot.
	// Вызываться должно после Start().
	//
	// надо помнить, что если больше не нужно отслеживать дескриптор, то нужно вызвать Stop() для того,
	// чтобы избежать утечки
	Resume(*Desc) error
}

// CallbackFn is a function that will be called on kernel i/o event
// notification.
type CallbackFn func(Event)

// Config contains options for Poller configuration.
type Config struct {
	// OnWaitError will be called from goroutine, waiting for events.
	OnWaitError func(error)
}

func (c *Config) withDefaults() (config Config) {
	if c != nil {
		config = *c
	}
	if config.OnWaitError == nil {
		config.OnWaitError = defaultOnWaitError
	}
	return config
}

func defaultOnWaitError(err error) {
	log.Printf("netpoll: wait loop error: %s", err)
}
