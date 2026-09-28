package telegram

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gotd/td/tg"
)

// Kind - вид адресата. Тремя видами Telegram и ограничивается: человек,
// обычная группа, супергруппа или канал.
type Kind string

const (
	KindUser    Kind = "user"
	KindChat    Kind = "chat"    // обычная группа
	KindChannel Kind = "channel" // супергруппа или канал
)

// Ref - ссылка на адресата в виде `user:123`, `chat:123`, `channel:123`.
//
// Строка, а не структура, потому что этот идентификатор живёт в БД, в ключах
// очереди и в логах. Каноническая запись без ведущих нулей: иначе один адресат
// получил бы два разных ключа очереди и два «первых» сообщения.
type Ref string

// NewRef собирает ссылку на адресата.
func NewRef(kind Kind, id int64) Ref {
	return Ref(string(kind) + ":" + strconv.FormatInt(id, 10))
}

// Split разбирает ссылку. Ошибка означает, что данные испорчены на стороне
// сервиса, а не отказ Telegram.
func (r Ref) Split() (Kind, int64, error) {
	kind, rawID, ok := strings.Cut(string(r), ":")
	if !ok || rawID == "" {
		return "", 0, fmt.Errorf("invalid peer ref %q", string(r))
	}
	switch Kind(kind) {
	case KindUser, KindChat, KindChannel:
	default:
		return "", 0, fmt.Errorf("invalid peer ref %q", string(r))
	}
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != rawID {
		return "", 0, fmt.Errorf("invalid peer ref %q", string(r))
	}
	return Kind(kind), id, nil
}

// UserID разбирает ссылку, которая обязана указывать на человека. Нужен там,
// где групповой адресат - это ошибка данных, а не другой сценарий.
func (r Ref) UserID() (int64, error) {
	kind, id, err := r.Split()
	if err != nil {
		return 0, err
	}
	if kind != KindUser {
		return 0, fmt.Errorf("peer ref %q is not a user", string(r))
	}
	return id, nil
}

// refFromPeer переводит адресата из апдейта в ссылку.
func refFromPeer(peer tg.PeerClass) (Ref, bool) {
	switch value := peer.(type) {
	case *tg.PeerUser:
		if value.UserID <= 0 {
			return "", false
		}
		return NewRef(KindUser, value.UserID), true
	case *tg.PeerChat:
		if value.ChatID <= 0 {
			return "", false
		}
		return NewRef(KindChat, value.ChatID), true
	case *tg.PeerChannel:
		if value.ChannelID <= 0 {
			return "", false
		}
		return NewRef(KindChannel, value.ChannelID), true
	default:
		return "", false
	}
}
