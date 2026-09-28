package telegram

import (
	"context"
	"errors"
	"time"

	"github.com/gotd/contrib/storage"
	"github.com/gotd/td/tgerr"
)

// Failure - ошибка Telegram, разобранная на «повторять или нет».
//
// Сервису важен не текст ошибки, а решение: отложить работу, бросить её
// навсегда или поднять тревогу. Поэтому разбор живёт здесь, один раз, а не
// расползается по местам вызова в виде сравнений строк.
type Failure struct {
	// Kind - причина одним словом, годится для лога и метрики.
	Kind string
	// Message - что случилось, человеческим языком.
	Message string
	// Type - код ошибки MTProto (MSG_ID_INVALID, FLOOD_WAIT) или пусто, если
	// ошибка не от Telegram. Без него вызывающий видит «временная ошибка
	// Telegram» и не знает, что именно ответил сервер.
	Type string
	// Permanent - повтор не поможет: та же попытка даст ту же ошибку.
	Permanent bool
	// RetryAt - не раньше этого момента, если Telegram назвал срок сам.
	RetryAt time.Time
}

// NotSent отвечает на единственный вопрос вызывающего после отказа: известно ли,
// что сообщения нет. Ответ даёт вид отказа, а не наличие кода: сервер разобрал
// запрос и выполнять отказался (ограничение, закрытая личка, кривой запрос,
// мёртвая сессия) либо до сервера дело не дошло вовсе - решил сам клиент или
// отказало локальное хранилище адресатов.
//
// Неизвестно ровно одно - KindTemporary. Туда падают обрыв, таймаут и любой
// неопознанный код, а неопознанный код - это в первую очередь 5xx
// (RPC_CALL_FAIL, INTERNAL_SERVER_ERROR, -503 Timeout): сервер мог создать
// сообщение и упасть уже после этого. Считать такой отказ «сообщения нет»
// значит написать человеку второй раз.
func (f *Failure) NotSent() bool {
	return f.Kind != KindTemporary
}

func (f *Failure) Error() string {
	if f.Type == "" {
		return f.Kind + ": " + f.Message
	}
	return f.Kind + ": " + f.Message + " (" + f.Type + ")"
}

// Виды ошибок. Список закрытый: новая строка здесь означает новое решение
// сервиса, а не новый текст в логе.
const (
	KindNotReady      = "telegram_not_ready"      // клиент не запущен
	KindFloodWait     = "telegram_flood_wait"     // Telegram просит подождать
	KindPeerFlood     = "telegram_peer_flood"     // аккаунт ограничен в письмах незнакомым
	KindPeerForbidden = "telegram_peer_forbidden" // писать этому адресату нельзя
	KindNotPerson     = "telegram_not_person"     // ник занят группой или каналом
	KindMediaInvalid  = "telegram_media_invalid"  // вложение отвергнуто
	KindAuthForbidden = "telegram_auth_forbidden" // сессия больше не авторизована
	KindBadRequest    = "telegram_bad_request"    // кривой запрос сервиса
	KindStorage       = "telegram_storage"        // своё хранилище адресатов отказало до вызова
	KindTemporary     = "telegram_temporary"      // прочее, лечится повтором
)

func permanent(kind, message string) *Failure {
	return &Failure{Kind: kind, Message: message, Permanent: true}
}

// classify переводит ошибку gotd в решение. Всё, что не опознано, считается
// временным: неизвестная ошибка, брошенная навсегда, тихо теряет сообщение, а
// лишний повтор стоит одной попытки.
func classify(err error) error {
	if err == nil {
		return nil
	}
	// Остановка сервиса ошибкой Telegram не является.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	failure := reason(err)
	if rpc, ok := tgerr.As(err); ok {
		failure.Type = rpc.Type
	}
	return failure
}

// reason - решение сервиса по ошибке, без кода MTProto: его проставляет
// classify, один раз на все ветки.
func reason(err error) *Failure {
	// PEER_FLOOD отдельной веткой: AsFloodWait его не узнаёт (матчит только
	// FLOOD_WAIT и FLOOD_PREMIUM_WAIT), срока Telegram не называет, и это не
	// пауза, а ограничение аккаунта на письма незнакомым - решать его повтором
	// нельзя.
	if tgerr.Is(err, "PEER_FLOOD") {
		return permanent(KindPeerFlood, "аккаунт ограничен в письмах незнакомым")
	}
	if wait, ok := tgerr.AsFloodWait(err); ok {
		return &Failure{
			Kind:    KindFloodWait,
			Message: "Telegram просит отложить повтор",
			RetryAt: time.Now().Add(wait),
		}
	}
	if errors.Is(err, storage.ErrPeerNotFound) || tgerr.Is(err,
		"PEER_ID_INVALID",
		"CHAT_WRITE_FORBIDDEN",
		"USER_IS_BLOCKED",
		"USER_PRIVACY_RESTRICTED",
		"INPUT_USER_DEACTIVATED",
	) {
		return permanent(KindPeerForbidden, "адресат недоступен для отправки")
	}
	// Отказ по самому файлу повтором не лечится: байты от попытки к попытке те
	// же. Без этой ветки отправка тратила бы три загрузки файла и паузы между
	// ними, чтобы прийти к тому же результату.
	if tgerr.Is(err,
		"MEDIA_INVALID",
		"MEDIA_EMPTY",
		"PHOTO_INVALID_DIMENSIONS",
		"PHOTO_EXT_INVALID",
		"STICKER_FILE_INVALID",
		"VIDEO_FILE_INVALID",
		"FILE_PARTS_INVALID",
	) {
		return permanent(KindMediaInvalid, "Telegram отверг вложение")
	}
	// Кривой запрос: пустой или слишком короткий поиск, несуществующий id
	// сообщения, отвергнутый текст. Повтор той же просьбы даст тот же отказ, и
	// считать её временной значит крутить его до исчерпания попыток.
	//
	// Отказ по тексту стоит здесь же, и это не мелочь: сообщение заведомо не
	// создано, а «повторим позже» оставляет строку журнала в pending навсегда,
	// закрывая человека для всех кампаний. MESSAGE_TOO_LONG приходит на живой
	// длине: своя проверка считает руны, Telegram - единицы UTF-16.
	if tgerr.Is(err,
		"MSG_ID_INVALID",
		"QUERY_TOO_SHORT",
		"SEARCH_QUERY_EMPTY",
		"OFFSET_PEER_ID_INVALID",
		"MESSAGE_TOO_LONG",
		"MESSAGE_EMPTY",
	) {
		return permanent(KindBadRequest, "Telegram отверг запрос")
	}
	// Мёртвая сессия - не «временная ошибка сети»: сервис обязан остановиться и
	// позвать человека, иначе он будет молча ретраить до бесконечности.
	if tgerr.Is(err,
		"AUTH_KEY_UNREGISTERED",
		"AUTH_KEY_INVALID",
		"SESSION_EXPIRED",
		"SESSION_REVOKED",
		"USER_DEACTIVATED",
		"USER_DEACTIVATED_BAN",
		"FROZEN_PARTICIPANT_MISSING",
	) {
		return permanent(KindAuthForbidden, "сессия больше не авторизована")
	}
	return &Failure{Kind: KindTemporary, Message: "временная ошибка Telegram"}
}
