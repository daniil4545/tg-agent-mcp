package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/daniil4545/tg-agent-mcp/internal/telegram"
)

// Transport - граница сервиса наружу: найти адресата по нику и отправить ему
// текст. Интерфейс - осознанное исключение из правила «без преждевременных
// интерфейсов»: вторая реализация (dry-run) существует с первого дня и
// остаётся навсегда режимом dev-контура.
type Transport interface {
	Resolve(ctx context.Context, username string) (Peer, error)
	// Send отдаёт id отправленного сообщения: он ложится в журнал ручных
	// касаний. Ноль означает «Telegram id не назвал» - так выглядит дубль по
	// random_id, и выдумывать вместо нуля значение нельзя.
	Send(ctx context.Context, peer Peer, text string, randomID int64) (int, error)
}

// Peer - найденный адресат. Ник лежит рядом со ссылкой, потому что на нём
// стоит последний рубеж allow-list, а ссылка Telegram ника уже не содержит.
type Peer struct {
	Username string
	Ref      telegram.Ref
}

// Dialog расширяет Transport чтением и поиском поверх той же сессии:
// ручная переписка и приёмка диалогов ботов портфеля. Встраивание, а не
// отдельный интерфейс, оставляет Transport нетронутым - Sender по-прежнему
// видит только Resolve/Send, и stubTransport в sender_test.go не обрастает
// пустыми методами.
//
// Lookup существует отдельно от Resolve намеренно: Resolve несёт рубеж
// checkAllowed (DEV_ALLOW_LIST), через него идёт кампанийная и ручная
// отправка живому человеку, и снимать рубеж нельзя. Но чтение диалога обязано
// работать и при непустом allow-list - иначе dev-контур не даёт прочитать
// диалог с тем, кому уже нельзя писать. Разделение снимает этот конфликт:
// Lookup рубеж не применяет, Resolve остаётся как есть. Не разделяя эти два
// пути, следующая правка либо оставляет чтение неработающим в dev, либо
// снимает checkAllowed из Resolve - а это дыра в живой отправке. Тест
// TestAllowListStillHoldsCampaignSend закрывает второй риск.
type Dialog interface {
	Transport
	// Lookup - резолв адресата для чтения, без рубежа allow-list.
	Lookup(ctx context.Context, username string) (Peer, error)
	Find(ctx context.Context, query string, limit int) ([]Contact, error)
	History(ctx context.Context, peer Peer, limit int) ([]ChatMessage, error)
	Dialogs(ctx context.Context, limit int) ([]DialogInfo, error)
}

// Contact - найденный при поиске человек, ещё не обязательно адресат. Ссылки
// на него здесь нет намеренно: отправка и чтение резолвят ник заново, а поле,
// похожее на готовый адрес, звало бы писать по нему без резолва.
type Contact struct {
	Username string
	Name     string
	// IsContact - человек в адресной книге аккаунта.
	IsContact bool
	// Known - адрес уже был известен сервису. Признака диалога это не даёт:
	// туда же попадает ник, резолвленный под чтение.
	Known bool
	// Exact - ник равен запросу без учёта регистра: это тот, кого искали, а не
	// похожий ник другого человека.
	Exact bool
}

// ChatMessage - сообщение диалога в плоском виде для ответа агенту.
type ChatMessage struct {
	ID        int
	Text      string
	MediaKind string
	SentAt    time.Time
	Outgoing  bool
}

// DialogInfo - строка списка диалогов с последним сообщением.
type DialogInfo struct {
	Username string
	Name     string
	Ref      telegram.Ref
	Unread   int
	// Broadcast - канал вещания, а не место для переписки. Голый факт платформы;
	// слово для выдачи собирает mcp.go из вида Ref и этого флага.
	Broadcast bool
	Last      ChatMessage
}

// ErrNotAllowed - отказ последнего рубежа: ник вне allow-list.
var ErrNotAllowed = errors.New("recipient is not in the allow list")

// ReasonNotAllowed - причина строки, снятой allow-list.
const ReasonNotAllowed = "not_allowed"

// Delivery - зафиксированная отправка dry-run: что и кому ушло бы живьём.
type Delivery struct {
	Username string
	Text     string
	RandomID int64
}

// DryRunTransport - транспорт dev-контура: пишет отправку в лог и запоминает
// вызовы, наружу не ходит вовсе. Живому человеку из него не уйдёт ни одного
// сообщения, поэтому сквозной прогон кампании безопасен.
type DryRunTransport struct {
	allowed map[string]bool

	mu   sync.Mutex
	sent []Delivery
}

// NewDryRunTransport собирает dry-run с рубежом allow-list из конфига.
func NewDryRunTransport(cfg Config) *DryRunTransport {
	return &DryRunTransport{allowed: allowedSet(cfg.DevAllowList)}
}

func (t *DryRunTransport) Resolve(_ context.Context, username string) (Peer, error) {
	if err := checkAllowed(t.allowed, username); err != nil {
		return Peer{}, err
	}
	return Peer{Username: username}, nil
}

// Send dry-run отдаёт синтетический возрастающий id - номер отправки в
// собственном журнале: живого id нет, а строка журнала с NULL не отличима от
// сбоя записи.
func (t *DryRunTransport) Send(_ context.Context, peer Peer, text string, randomID int64) (int, error) {
	if err := checkAllowed(t.allowed, peer.Username); err != nil {
		return 0, err
	}
	t.mu.Lock()
	t.sent = append(t.sent, Delivery{Username: peer.Username, Text: text, RandomID: randomID})
	messageID := len(t.sent)
	t.mu.Unlock()

	slog.Info("dry-run send", "username", peer.Username, "random_id", randomID, "text", text)
	return messageID, nil
}

// Sent отдаёт копию зафиксированных отправок.
func (t *DryRunTransport) Sent() []Delivery {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Delivery(nil), t.sent...)
}

// Lookup dry-run не ходит никуда - живого Telegram нет вовсе, поэтому
// возвращает подставного Peer по нику без рубежа. Честность подставного
// ответа держит инструмент MCP отдельной строкой в тексте.
func (t *DryRunTransport) Lookup(_ context.Context, username string) (Peer, error) {
	return Peer{Username: username}, nil
}

// Find dry-run отдаёт один синтетический контакт по запросу: реального
// поиска нет, а формат ответа обязан совпадать с живым транспортом. Ник
// нормализуется тем же CleanQuery, что и живой поиск, и запрос по форме ника
// помечается точным: иначе ответ предупреждает «точного ника нет» про ник,
// который сам же и вернул.
func (t *DryRunTransport) Find(_ context.Context, query string, _ int) ([]Contact, error) {
	needle := telegram.CleanQuery(query)
	return []Contact{{Username: needle, Name: query, Exact: telegram.IsUsername(needle)}}, nil
}

// History dry-run отдаёт собственный журнал t.sent как исходящие сообщения -
// единственная история, которая у dry-run есть на самом деле, входящих не
// бывает.
func (t *DryRunTransport) History(_ context.Context, peer Peer, limit int) ([]ChatMessage, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var history []ChatMessage
	for _, delivery := range t.sent {
		if delivery.Username != peer.Username {
			continue
		}
		history = append(history, ChatMessage{Text: delivery.Text, Outgoing: true})
	}
	if limit > 0 && len(history) > limit {
		history = history[len(history)-limit:]
	}
	return history, nil
}

// Dialogs dry-run отдаёт пусто: список диалогов живому аккаунту не
// подделать честно, в отличие от History, где есть свой журнал отправок.
func (t *DryRunTransport) Dialogs(_ context.Context, _ int) ([]DialogInfo, error) {
	return nil, nil
}

// LiveTransport - живой MTProto поверх пакета telegram. В этом срезе не
// запускается: конфиг не даёт поднять TRANSPORT=live вне prod.
type LiveTransport struct {
	allowed map[string]bool
	client  *telegram.Client
}

// NewLiveTransport собирает живой транспорт с тем же рубежом allow-list.
func NewLiveTransport(cfg Config, client *telegram.Client) *LiveTransport {
	return &LiveTransport{allowed: allowedSet(cfg.DevAllowList), client: client}
}

func (t *LiveTransport) Resolve(ctx context.Context, username string) (Peer, error) {
	if err := checkAllowed(t.allowed, username); err != nil {
		return Peer{}, err
	}
	ref, err := t.client.ResolveUsername(ctx, username)
	if err != nil {
		return Peer{}, err
	}
	return Peer{Username: username, Ref: ref}, nil
}

// Send отдаёт исход как есть: узнанный Telegram random_id - это успех, клиент
// уже вернул его без ошибки, и второго сообщения человек не получил. Id такого
// дубля нулевой, и ноль уходит в журнал как есть.
func (t *LiveTransport) Send(ctx context.Context, peer Peer, text string, randomID int64) (int, error) {
	if err := checkAllowed(t.allowed, peer.Username); err != nil {
		return 0, err
	}
	result, err := t.client.SendText(ctx, peer.Ref, text, randomID)
	return result.MessageID, err
}

// Lookup резолвит адресата для чтения без рубежа allow-list - см. комментарий
// на интерфейсе Dialog. Отправка через этот Peer не идёт, поэтому обходить
// checkAllowed здесь безопасно.
func (t *LiveTransport) Lookup(ctx context.Context, username string) (Peer, error) {
	ref, err := t.client.ResolveUsername(ctx, username)
	if err != nil {
		return Peer{}, err
	}
	return Peer{Username: username, Ref: ref}, nil
}

func (t *LiveTransport) Find(ctx context.Context, query string, limit int) ([]Contact, error) {
	found, err := t.client.SearchUsers(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	contacts := make([]Contact, 0, len(found))
	for _, f := range found {
		contacts = append(contacts, Contact{
			Username:  f.Username,
			Name:      f.Name,
			IsContact: f.IsContact,
			Known:     f.Known,
			Exact:     f.Exact,
		})
	}
	return contacts, nil
}

// History берёт новейшую страницу истории: beforeID=0 у клиента telegram
// означает «от последнего сообщения».
func (t *LiveTransport) History(ctx context.Context, peer Peer, limit int) ([]ChatMessage, error) {
	messages, err := t.client.History(ctx, peer.Ref, limit, 0)
	if err != nil {
		return nil, err
	}
	history := make([]ChatMessage, 0, len(messages))
	for _, m := range messages {
		history = append(history, chatMessageFrom(m))
	}
	return history, nil
}

func (t *LiveTransport) Dialogs(ctx context.Context, limit int) ([]DialogInfo, error) {
	dialogs, err := t.client.Dialogs(ctx, limit)
	if err != nil {
		return nil, err
	}
	result := make([]DialogInfo, 0, len(dialogs))
	for _, d := range dialogs {
		result = append(result, DialogInfo{
			Username:  d.Username,
			Name:      d.Name,
			Ref:       d.Peer,
			Unread:    d.Unread,
			Broadcast: d.Broadcast,
			Last:      chatMessageFrom(d.Last),
		})
	}
	return result, nil
}

// chatMessageFrom приводит сообщение пакета telegram к плоскому виду
// ChatMessage - общий шаг History и последнего сообщения в Dialogs.
func chatMessageFrom(m telegram.Message) ChatMessage {
	return ChatMessage{ID: m.ID, Text: m.Text, MediaKind: m.MediaKind, SentAt: m.SentAt, Outgoing: m.Outgoing}
}

// allowedSet - множество разрешённых ников; nil означает «ограничения нет».
func allowedSet(list []string) map[string]bool {
	if len(list) == 0 {
		return nil
	}
	set := make(map[string]bool, len(list))
	for _, name := range list {
		set[name] = true
	}
	return set
}

// checkAllowed - последний рубеж перед выходом наружу, за всеми рубежами БД.
// Пустой список никого не ограничивает (боевая рассылка идёт по списку из
// кампании), непустой пропускает только перечисленных - и живой транспорт, и
// dry-run одинаково: рубеж не должен зависеть от того, каким транспортом
// подняли процесс.
func checkAllowed(allowed map[string]bool, username string) error {
	if allowed == nil || allowed[normalizeUsername(username)] {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrNotAllowed, username)
}
