package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/daniil4545/tg-agent-mcp/internal/telegram"
)

// Две группы инструментов поверх одного аккаунта: шесть кампанийных ходят
// только в базу, четыре диалоговых - через Chat в живую сессию. Кампания в
// системе одна, поэтому id никуда не передаётся - единственность держат
// частичные индексы БД.
type tools struct {
	service *Service
	chat    *Chat
}

type loadArgs struct {
	Template string `json:"template" jsonschema:"текст приглашения; {имя} заменяется на имя получателя"`
	Title    string `json:"title" jsonschema:"название кампании для отчётов"`
}

type personArgs struct {
	ContactID int64  `json:"contact_id" jsonschema:"id контакта amoCRM"`
	LeadID    int64  `json:"lead_id,omitempty" jsonschema:"id сделки amoCRM"`
	Name      string `json:"name" jsonschema:"имя для подстановки в шаблон"`
	Username  string `json:"username" jsonschema:"ник Telegram; без ника человек в рассылку не попадёт"`
}

type addArgs struct {
	People []personArgs `json:"people" jsonschema:"пачка получателей, не больше 50 за вызов"`
}

// omitempty у флагов не косметика: без него схема объявляет поле обязательным, и
// вызов без аргументов отбивается проверкой схемы, не дойдя до обработчика.
type stopArgs struct {
	Cancel bool `json:"cancel,omitempty" jsonschema:"true - отменить кампанию вместе с очередью; false (по умолчанию) - пауза, start продолжит"`
}

type resumeArgs struct {
	Label string `json:"label" jsonschema:"метка аккаунта, с которого снимается стоп"`
}

type statusArgs struct {
	Full       bool  `json:"full,omitempty" jsonschema:"добавить построчный итог по получателям; без значения отчёт идёт без него"`
	CampaignID int64 `json:"campaign_id,omitempty" jsonschema:"id кампании из load_campaign; без него - текущая: идущая, черновик, пауза, иначе последняя закрытая"`
}

type findArgs struct {
	Query string `json:"query" jsonschema:"ник ищется точно, точное совпадение идёт первым с пометкой; без него показываются похожие ники с предупреждением. Имя ищется среди контактов и своих диалогов"`
}

type sendArgs struct {
	Username string `json:"username" jsonschema:"ник Telegram без @"`
	Text     string `json:"text" jsonschema:"текст сообщения: непустой, не длиннее 4096 символов"`
}

type readArgs struct {
	Username    string `json:"username" jsonschema:"ник Telegram без @"`
	Depth       int    `json:"depth,omitempty" jsonschema:"сколько последних сообщений показать; без значения двадцать, потолок сто"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"сколько секунд ждать чужую реплику; без значения не ждать вовсе, потолок 45 секунд - дольше вызов не переживает клиент. Нужно ждать дольше - зовите tg_read повторно, ожидание продолжится с того же места"`
}

type dialogsArgs struct {
	Limit int `json:"limit,omitempty" jsonschema:"сколько диалогов показать; без значения двадцать, потолок сто"`
}

// NewMCPServer собирает MCP-сервер управления кампанией.
func NewMCPServer(service *Service, chat *Chat) *mcp.Server {
	handlers := &tools{service: service, chat: chat}
	server := mcp.NewServer(&mcp.Implementation{Name: "tg-agent-mcp", Version: "1.0.0"}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "load_campaign",
		Description: "Создать черновик рассылки с шаблоном сообщения. Прежний черновик отменяется.",
	}, handlers.loadCampaign)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "add_recipients",
		Description: "Добавить пачку получателей (до 50) в черновик и получить отчёт отсева с превью сообщения. Для служебных адресатов из DEV_ALLOW_LIST правило «одно сообщение» не действует, стоп-лист - действует.",
	}, handlers.addRecipients)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "start",
		Description: "Запустить черновик или продолжить кампанию с паузы.",
	}, handlers.start)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "stop",
		Description: "Поставить кампанию на паузу или отменить её вместе с очередью.",
	}, handlers.stop)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "resume_account",
		Description: "Снять стоп с аккаунта после проверки человеком.",
	}, handlers.resumeAccount)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "status",
		Description: "Счётчики кампании, аккаунты со стопами и превью сообщения; campaign_id показывает названную кампанию, full добавляет построчный итог.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handlers.status)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "tg_find",
		Description: "Найти человека: точный ник или имя среди контактов и своих диалогов. Точное совпадение ника идёт первым с пометкой, без него показываются похожие ники с предупреждением. Наружу поиск ничего не пишет.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handlers.find)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "tg_send",
		Description: "Написать человеку в личку руками. Первое касание незнакомца идёт в окно отправки и в дневной потолок; после отправки человек закрыт для кампаний. Для служебных адресатов из DEV_ALLOW_LIST окно, потолок и правило «одно сообщение» не действуют, стоп-лист - действует.",
	}, handlers.send)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "tg_read",
		Description: "Показать личный диалог и, если попросили, дождаться чужой реплики; группы и каналы не читаются.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handlers.read)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "tg_dialogs",
		Description: "Список диалогов - личных, групп и каналов - с непрочитанными и последним сообщением; вид каждой строки назван, писать (tg_send) можно только человеку.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handlers.dialogs)

	return server
}

// Роли токенов. UserID обязан быть непустым: по нему SDK привязывает сессию к
// токену, и пустое значение эту привязку выключает.
const (
	roleOwner = "owner"
	roleAgent = "agent"
)

// MCPHandler - эндпоинт управления: streamable HTTP за bearer-токеном. Роли
// две: владелец видит все инструменты и подтверждает мутации на стороне Клода,
// агент видит два диалоговых, и рубеж его адресатов держит сервис.
func MCPHandler(service *Service, chat *Chat) http.Handler {
	owner := NewMCPServer(service, chat)
	agent := NewAgentServer(service, chat)
	streamable := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		info := auth.TokenInfoFromContext(req.Context())
		if info == nil {
			return nil
		}
		switch info.UserID {
		case roleOwner:
			return owner
		case roleAgent:
			return agent
		}
		return nil
	}, nil)
	// AllowMissingExpiration: токен статический и срока в себе не несёт;
	// без этого флага middleware отклоняет его как просроченный.
	verify := staticTokens(service.cfg.MCPToken, service.cfg.MCPAgentToken)
	guard := auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})
	return guard(streamable)
}

// staticTokens сверяет предъявленный токен с настроенными за постоянное время:
// посимвольное сравнение выдаёт токен по времени ответа. Пустой токен агента
// роль не открывает.
func staticTokens(owner, agent string) auth.TokenVerifier {
	return func(_ context.Context, presented string, _ *http.Request) (*auth.TokenInfo, error) {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(owner)) == 1 {
			return &auth.TokenInfo{UserID: roleOwner}, nil
		}
		if agent != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(agent)) == 1 {
			return &auth.TokenInfo{UserID: roleAgent}, nil
		}
		// Отказ авторизации виден только снаружи: клиент получает 401 без
		// объяснения, и без этой записи причину не отличить от лежащего
		// сервиса. Токен не пишется, только его длина - разошедшийся токен
		// обычно пуст или обрезан.
		slog.Warn("mcp token rejected", "presented_len", len(presented))
		return nil, auth.ErrInvalidToken
	}
}

// errAgentRecipient - отказ агентского доступа: адресат не тестовый аккаунт.
var errAgentRecipient = errors.New("ник вне AGENT_ALLOW_LIST: агентский доступ пишет и читает только тестовые аккаунты")

// agentTools - диалоговые инструменты за списком тестовых аккаунтов. Ник
// сверяется до Chat той же нормализацией, что в Chat: иначе запись с @ или
// ссылкой разошлась бы со списком.
type agentTools struct {
	tools   *tools
	allowed map[string]bool
}

func (a *agentTools) check(username string) error {
	if !a.allowed[normalizeUsername(username)] {
		slog.Warn("agent recipient refused", "username", username)
		return errAgentRecipient
	}
	return nil
}

func (a *agentTools) send(ctx context.Context, req *mcp.CallToolRequest, args sendArgs) (*mcp.CallToolResult, any, error) {
	if err := a.check(args.Username); err != nil {
		return nil, nil, err
	}
	return a.tools.send(ctx, req, args)
}

func (a *agentTools) read(ctx context.Context, req *mcp.CallToolRequest, args readArgs) (*mcp.CallToolResult, any, error) {
	if err := a.check(args.Username); err != nil {
		return nil, nil, err
	}
	return a.tools.read(ctx, req, args)
}

// NewAgentServer собирает сервер агентского доступа. Новый инструмент сюда
// не попадает сам: агенту он открывается только осознанно.
func NewAgentServer(service *Service, chat *Chat) *mcp.Server {
	handlers := &agentTools{
		tools:   &tools{service: service, chat: chat},
		allowed: allowedSet(service.cfg.AgentAllowList),
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "tg-agent-mcp-agent", Version: "1.0.0"}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "tg_send",
		Description: "Написать тестовому аккаунту из AGENT_ALLOW_LIST в личку, чтобы проверить бота. Любой другой ник сервис отклоняет.",
	}, handlers.send)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "tg_read",
		Description: "Показать личный диалог с тестовым аккаунтом из AGENT_ALLOW_LIST и, если попросили, дождаться его реплики.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handlers.read)

	return server
}

func (t *tools) loadCampaign(ctx context.Context, _ *mcp.CallToolRequest, args loadArgs) (*mcp.CallToolResult, any, error) {
	id, err := t.service.LoadCampaign(ctx, args.Template, args.Title)
	if err != nil {
		return nil, nil, toolError(err)
	}
	slog.Info("campaign loaded", "campaign_id", id, "title", strings.TrimSpace(args.Title))

	// Шаблон печатается щедро и с переносами строк: проверка «что уйдёт человеку»
	// глазами - единственный способ поймать удвоенную скобку в третьей строке.
	// Режется он только на длинных письмах, и длина называется всегда.
	template := strings.TrimSpace(args.Template)
	var text strings.Builder
	// Длина называется та же, что проверяет отказ: с запасом на имя и по счёту
	// Telegram. Голая длина шаблона обещала бы запас, которого нет, - дописанные
	// пятьдесят символов дали бы отказ с другой, большей цифрой.
	used, budget := renderedLen(template)
	fmt.Fprintf(&text, "Черновик создан: %s (id %d).\n", strings.TrimSpace(args.Title), id)
	if budget > 0 {
		fmt.Fprintf(&text, "Шаблон с подставленным именем занимает %d из %d символов по счёту Telegram (из них %d - запас на имя):\n%s\n\n",
			used, maxTextLen, budget, cutRunes(template, templateShown))
	} else {
		fmt.Fprintf(&text, "Шаблон занимает %d из %d символов по счёту Telegram:\n%s\n\n",
			used, maxTextLen, cutRunes(template, templateShown))
	}
	// Шаблон без подстановки законен, но молчаливая рассылка без обращения - не
	// то, что обычно имели в виду, и увидеть это надо до старта.
	if hasName(template) {
		fmt.Fprintf(&text, "Подстановка %s в шаблоне есть: получателю без имени письмо уйдёт без обращения, вместе с запятой. Проверьте, что текст читается и так.\n", namePlaceholder)
	} else {
		fmt.Fprintf(&text, "Подстановки %s в шаблоне нет: письма уйдут без обращения по имени.\n", namePlaceholder)
	}
	fmt.Fprintf(&text, "Дальше: add_recipients пачками до %d человек, затем start.", maxBatch)
	return textResult(text.String())
}

// addRecipients работает с единственным черновиком: id кампании инструмент не
// принимает, потому что черновик в системе один и его держит частичный индекс.
func (t *tools) addRecipients(ctx context.Context, _ *mcp.CallToolRequest, args addArgs) (*mcp.CallToolResult, any, error) {
	campaignID, err := t.service.draftCampaign(ctx)
	if err != nil {
		return nil, nil, toolError(err)
	}

	// personArgs повторяет Person поле в поле и отличается только тегами схемы:
	// доменный тип не обязан знать про формат аргументов инструмента.
	people := make([]Person, 0, len(args.People))
	for _, item := range args.People {
		people = append(people, Person(item))
	}
	report, err := t.service.AddRecipients(ctx, campaignID, people)
	if err != nil {
		return nil, nil, toolError(err)
	}

	slog.Info("recipients added", "campaign_id", campaignID,
		"offered", len(args.People), "accepted", report.Accepted, "skipped", report.Skipped)

	var text strings.Builder
	fmt.Fprintf(&text, "Принято: %d из %d.\n", report.Accepted, len(args.People))
	writeSkipped(&text, report.Skipped)

	// Отчёт строится по черновику, в который вставляли, а не по «текущей»
	// кампании: при живой активной сверка списка ушла бы по чужим счётчикам.
	status, err := t.service.StatusOf(ctx, campaignID, false)
	if err != nil {
		return nil, nil, err
	}
	writeCampaign(&text, status)
	writePreview(&text, status)
	return textResult(text.String())
}

func (t *tools) start(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	id, err := t.service.StartCampaign(ctx)
	if err != nil {
		return nil, nil, toolError(err)
	}
	slog.Info("campaign started", "campaign_id", id)
	status, err := t.service.StatusOf(ctx, id, false)
	if err != nil {
		return nil, nil, err
	}

	// Что именно запущено, называет отчёт ниже: при паре «черновик плюс пауза»
	// start берёт черновик, и оператор должен увидеть, какая кампания пошла и
	// что осталось ждать.
	var text strings.Builder
	fmt.Fprintf(&text, "Кампания запущена (id %d). Отправка идёт в окно %s-%s по будням, не больше дневного потолка аккаунта.\n",
		id, t.service.cfg.SendWindowStart, t.service.cfg.SendWindowEnd)
	writeCampaign(&text, status)
	// Строка выше - правило, эта - положение дел сейчас: запуск в 22:10 пятницы
	// иначе выглядит как пошедшая отправка. Причина уже посчитана в status,
	// запросов не прибавляется.
	writeIdle(&text, status)
	writeAccounts(&text, status)
	return textResult(text.String())
}

func (t *tools) stop(ctx context.Context, _ *mcp.CallToolRequest, args stopArgs) (*mcp.CallToolResult, any, error) {
	id, err := t.service.StopCampaign(ctx, args.Cancel)
	if err != nil {
		return nil, nil, toolError(err)
	}
	slog.Info("campaign stopped", "campaign_id", id, "cancelled", args.Cancel)
	// Отчёт по той кампании, которую остановили: рядом может лежать черновик, и
	// «текущая» кампания в ответе была бы уже им.
	status, err := t.service.StatusOf(ctx, id, false)
	if err != nil {
		return nil, nil, err
	}

	var text strings.Builder
	if args.Cancel {
		text.WriteString("Кампания отменена, очередь снята: продолжить её больше нельзя.\n")
	} else {
		text.WriteString("Кампания на паузе. Отправок нет; start продолжит с того же места.\n")
	}
	writeCampaign(&text, status)
	return textResult(text.String())
}

func (t *tools) resumeAccount(ctx context.Context, _ *mcp.CallToolRequest, args resumeArgs) (*mcp.CallToolResult, any, error) {
	label := strings.TrimSpace(args.Label)
	// Состояние и снятие - одна транзакция под блокировкой строки: стоп,
	// поставленный между чтением и снятием, иначе снялся бы молча, а ответ и лог
	// назвали бы прежнюю причину. Ответ «стоп снят» на живом аккаунте тоже
	// невозможен - снимать или нет, решает та же транзакция.
	reason, stopped, err := t.service.ResumeAccount(ctx, label)
	if err != nil {
		return nil, nil, t.unknownAccount(ctx, label, err)
	}
	if !stopped {
		// Про отправку тут ничего не обещается: транзакция смотрела один стоп, а
		// молчать очередь может из-за окна, потолка или отсутствия кампании.
		return textResult(fmt.Sprintf("Стопа на аккаунте %q нет: снимать нечего. Идёт ли отправка, покажет status.", label))
	}
	// Снятие стопа - единственное действие, которое человек делает руками после
	// разбора аккаунта: в логе оно должно стоять рядом с самим стопом.
	slog.Info("account resumed", "account", label, "stop_reason", reason)
	return textResult(fmt.Sprintf("Стоп с аккаунта %q снят (стоял из-за %s). Отправка возобновится в ближайшее окно.",
		label, reason))
}

func (t *tools) status(ctx context.Context, _ *mcp.CallToolRequest, args statusArgs) (*mcp.CallToolResult, any, error) {
	var status StatusReport
	var err error
	// Любой ненулевой id идёт в StatusOf, включая отрицательный: «параметр не
	// передан» - это только ноль, а отчёт о текущей кампании в ответ на явную
	// опечатку в id читается как отчёт о запрошенной.
	if args.CampaignID != 0 {
		status, err = t.service.StatusOf(ctx, args.CampaignID, args.Full)
	} else {
		status, err = t.service.Status(ctx, args.Full)
	}
	if err != nil {
		return nil, nil, err
	}
	// StatusOf по чужому id отдаёт отчёт без кампании: аккаунты вместо
	// запрошенного итога читаются как «кампания пуста», а не как опечатка в id.
	if args.CampaignID != 0 && status.Campaign == nil {
		return nil, nil, fmt.Errorf("кампании с id %d нет: id называет ответ load_campaign, без campaign_id status покажет текущую",
			args.CampaignID)
	}

	var text strings.Builder
	writeCampaign(&text, status)
	writeIdle(&text, status)
	writePreview(&text, status)
	writeAccounts(&text, status)
	if args.Full {
		writeRows(&text, status)
	}
	return textResult(text.String())
}

func (t *tools) find(ctx context.Context, _ *mcp.CallToolRequest, args findArgs) (*mcp.CallToolResult, any, error) {
	found, err := t.chat.Find(ctx, args.Query)
	if err != nil {
		return nil, nil, toolError(err)
	}
	var text strings.Builder
	t.writeContour(&text)
	// Запрос приводится к тому же виду, в каком его искал telegram.SearchUsers:
	// сырой «@Ivan» не прошёл бы проверку формы ника, а в тексте дал бы «@@».
	needle := telegram.CleanQuery(args.Query)
	writeContacts(&text, needle, found)
	return textResult(text.String())
}

// send отчитывается ценой касания, а не фактом отправки: журнал закрывает
// человека для всех будущих кампаний, и решение об этом принимает вызывающий.
func (t *tools) send(ctx context.Context, _ *mcp.CallToolRequest, args sendArgs) (*mcp.CallToolResult, any, error) {
	sent, err := t.chat.Send(ctx, args.Username, args.Text)
	if err != nil {
		return nil, nil, toolError(err)
	}

	var text strings.Builder
	t.writeContour(&text)
	fmt.Fprintf(&text, "Отправлено @%s", sent.Username)
	if sent.UserID > 0 {
		fmt.Fprintf(&text, " (id %d)", sent.UserID)
	}
	text.WriteString(".\nКасание записано: этот человек больше не попадёт ни в одну кампанию.\n")
	if sent.Cold {
		text.WriteString("Это первое касание незнакомца: оно ушло в дневной потолок аккаунта.\n")
	}
	text.WriteString("Дальше: tg_read с wait_seconds дождётся ответа.\n")
	return textResult(text.String())
}

func (t *tools) read(ctx context.Context, _ *mcp.CallToolRequest, args readArgs) (*mcp.CallToolResult, any, error) {
	talk, err := t.chat.Read(ctx, args.Username, args.Depth, time.Duration(args.WaitSeconds)*time.Second)
	if err != nil {
		return nil, nil, toolError(err)
	}
	var text strings.Builder
	t.writeContour(&text)
	// Счёт идёт по истории, а не по всему напечатанному: глубина резала именно
	// её, а дождавшаяся реплика пришла сверх среза.
	writeHistoryCap(&text, args.Depth, len(talk.Messages))
	// Про само ожидание строка молчит: сколько ждали на деле, ниже говорит
	// writeConversation, и «ждали 45» рядом с «реплика через 6 секунд» - два
	// взаимоисключающих числа подряд.
	if args.WaitSeconds > int(maxWait.Seconds()) {
		fmt.Fprintf(&text, "Просили ждать %d секунд, потолок одного вызова - %d: дольше вызов не переживает клиент.\n",
			args.WaitSeconds, int(maxWait.Seconds()))
	}
	writeConversation(&text, talk)
	return textResult(text.String())
}

func (t *tools) dialogs(ctx context.Context, _ *mcp.CallToolRequest, args dialogsArgs) (*mcp.CallToolResult, any, error) {
	list, err := t.chat.Dialogs(ctx, args.Limit)
	if err != nil {
		return nil, nil, toolError(err)
	}
	var text strings.Builder
	t.writeContour(&text)
	writeDialogsCap(&text, args.Limit, len(list))
	writeDialogs(&text, list)
	return textResult(text.String())
}

// writeHistoryCap называет срез чтения диалога. Оговорка нужна не только при
// явном depth больше потолка: самый частый вызов идёт без аргумента и режется
// умолчанием, а урезанный молча диалог читается как весь. Молчит она там, где
// история короче глубины: диалог показан целиком, и «показано не всё» отправляло
// бы агента искать несуществующий остаток.
func writeHistoryCap(text *strings.Builder, asked, shown int) {
	if asked > maxHistory {
		fmt.Fprintf(text, "Просили %d последних сообщений, потолок %d.\n", asked, maxHistory)
	}
	depth := capped(asked, defaultHistory, maxHistory)
	if shown < depth {
		return
	}
	if depth < maxHistory {
		fmt.Fprintf(text, "Показано не всё: глубина %d исчерпана, более ранние сообщения остались за срезом. "+
			"Нужно дальше - позовите tg_read с depth до %d.\n", depth, maxHistory)
		return
	}
	fmt.Fprintf(text, "Показано не всё: глубина %d исчерпана, более ранние сообщения остались за срезом, "+
		"глубже потолка инструмент не читает.\n", depth)
}

// writeDialogsCap называет срез списка диалогов: без этой строки «показано 12»
// читается как «всего 12», и агент делает вывод «человека в диалогах нет» по
// началу списка. Молчит там, где список короче предела: показаны все диалоги
// аккаунта, и совет позвать tg_dialogs с большим limit был бы ложным.
func writeDialogsCap(text *strings.Builder, asked, shown int) {
	if asked > maxHistory {
		fmt.Fprintf(text, "Просили %d диалогов, потолок %d.\n", asked, maxHistory)
	}
	limit := capped(asked, defaultHistory, maxHistory)
	if shown < limit {
		return
	}
	fmt.Fprintf(text, "Показано %d последних диалогов, дальше по списку могут быть ещё.", limit)
	if limit < maxHistory {
		fmt.Fprintf(text, " Нужно больше - позовите tg_dialogs с limit до %d.\n", maxHistory)
		return
	}
	text.WriteString(" Глубже потолка инструмент не смотрит: человека, которого не видно, ищите точным ником через tg_find.\n")
}

// unknownAccount переводит отказ чтения аккаунта. Незнакомая метка - его
// единственная причина, зависящая от вызывающего, и она обязана называть, где
// взять правильную; прочие отказы (база недоступна) уходят как есть, а не под
// видом опечатки в метке.
func (t *tools) unknownAccount(ctx context.Context, label string, err error) error {
	var known int
	if scanErr := t.service.pool.QueryRow(ctx,
		`SELECT count(*) FROM accounts WHERE label = $1`, label).Scan(&known); scanErr != nil || known > 0 {
		return toolError(err)
	}
	return fmt.Errorf("аккаунта с меткой %q сервис не знает: метки аккаунтов перечисляет status, позовите resume_account с одной из них",
		label)
}

// writeContour называет подставной контур первой строкой: dry-run отдаёт
// собственный журнал отправок как историю, и без этой строки агент принял бы
// его за настоящий диалог.
func (t *tools) writeContour(text *strings.Builder) {
	if t.service.cfg.Transport != "live" {
		text.WriteString("Контур dry-run: живого Telegram нет, ответ подставной.\n")
	}
}

// draftCampaign находит единственный черновик. Инструменты кампании id не
// передают: черновик в системе один, и его единственность держит частичный
// уникальный индекс, а не договорённость вызывающего.
func (s *Service) draftCampaign(ctx context.Context) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT id FROM campaigns WHERE status = 'draft'`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, withNear(ctx, s.pool, ErrNoDraft, "active", "stopped")
	}
	if err != nil {
		return 0, fmt.Errorf("find draft: %w", err)
	}
	return id, nil
}

// toolError переводит доменный отказ в текст, из которого оператору понятно,
// что делать дальше: иначе Клод показывает владельцу обёрнутую ошибку запроса.
func toolError(err error) error {
	switch {
	case errors.Is(err, ErrNoDraft):
		return noDraftError(err)
	case errors.Is(err, ErrNothingToStart):
		return errors.New("запускать нечего: нет ни черновика, ни кампании на паузе. Создайте черновик через load_campaign")
	case errors.Is(err, ErrNothingToStop):
		return nothingToStopError(err)
	case errors.Is(err, ErrNothingToCancel):
		return errors.New("отменять нечего: нет ни идущей кампании, ни черновика, ни паузы")
	case errors.Is(err, ErrActiveCampaign):
		return errors.New("кампания уже идёт: сначала остановите её через stop, потом запускайте следующую")
	case errors.Is(err, ErrManyCampaigns):
		return errors.New("кампаний для запуска больше одной: лишние снимите через stop с cancel")
	case errors.Is(err, ErrEmptyDraft):
		return errors.New("в черновике нет пригодных получателей: добавьте их через add_recipients")
	case errors.Is(err, ErrUnknownPlaceholder):
		return unknownPlaceholderError(err)
	case errors.Is(err, ErrTemplateTooLong):
		return longTemplateError(err)
	case errors.Is(err, ErrNotDraft):
		return errors.New("получателей принимает только черновик: запущенная кампания список не пополняет")
	case errors.Is(err, ErrBatchTooBig):
		return fmt.Errorf("в пачке больше %d человек: разбейте список на части", maxBatch)
	case errors.Is(err, ErrNoUsername):
		return errors.New("ник пустой или не похож на ник Telegram: 5-32 символа латиницей, цифры и подчёркивание, не в конце и не двойное; без @")
	case errors.Is(err, ErrEmptyText):
		return errors.New("текст пустой: назовите, что отправить")
	case errors.Is(err, ErrTextTooLong):
		return fmt.Errorf("сообщение длиннее %d символов: разбейте его на части", maxTextLen)
	case errors.Is(err, ErrNotPrivate):
		return errors.New("это не личный чат: инструменты работают только с личкой, группы и каналы не поддерживаются")
	case errors.Is(err, ErrAccountStopped):
		return accountStopError(err)
	case errors.Is(err, ErrStopList):
		return errors.New("человек в стоп-листе: ему не уходит ничего - ни кампанией, ни руками")
	case errors.Is(err, ErrCampaignSending):
		return campaignHoldError(err)
	case errors.Is(err, ErrDirectPending):
		return fmt.Errorf("предыдущее сообщение этому человеку в неизвестном состоянии: оно могло уйти. "+
			"Подождите %d минут и посмотрите диалог через tg_read", waitMinutes(err))
	case errors.Is(err, ErrColdWindow):
		return errors.New("первое касание незнакомца уходит только в окно отправки по будням: дождитесь окна; реплика в идущем диалоге временем не ограничена")
	case errors.Is(err, ErrColdLimit):
		return errors.New("дневной потолок аккаунта израсходован: первое касание незнакомца подождёт до завтра; реплика в идущем диалоге проходит")
	case errors.Is(err, ErrNotAllowed):
		return errors.New("ник вне DEV_ALLOW_LIST: в этом контуре писать можно только перечисленным")
	default:
		var failure *telegram.Failure
		if errors.As(err, &failure) {
			return telegramError(failure)
		}
		return err
	}
}

// unknownPlaceholderError перечисляет чужие подстановки: без списка человек
// ищет опечатку глазами по всему шаблону.
func unknownPlaceholderError(err error) error {
	var bad *badTemplate
	if !errors.As(err, &bad) {
		return fmt.Errorf("в шаблоне остались фигурные скобки, которых сервис не понимает: он знает одну подстановку - %s",
			namePlaceholder)
	}
	quoted := make([]string, 0, len(bad.spots))
	for _, spot := range bad.spots {
		quoted = append(quoted, fmt.Sprintf("%q", spot))
	}
	// Показаны не все места: без этой оговорки человек правит три, получает отказ
	// снова и читает его как новый дефект.
	shown := ""
	if bad.total > len(bad.spots) {
		shown = fmt.Sprintf(" (первые %d из %d)", len(bad.spots), bad.total)
	}
	// Совет зависит от того, где лежит шаблон: текст в аргументе правится на
	// месте, а шаблон существующей кампании не правит ни один инструмент, и
	// обещать «уберите скобки» там значит советовать невозможное.
	if bad.stored {
		return fmt.Errorf("шаблон этой кампании сервис не разобрал%s: %s. Отправки по ней не будет. "+
			"Шаблон существующей кампании не правится ни одним инструментом: соберите кампанию заново - "+
			"load_campaign с исправленным текстом, затем add_recipients (список придётся собрать заново), затем start; "+
			"негодную снимите через stop с cancel",
			shown, strings.Join(quoted, ", "))
	}
	return fmt.Errorf("в шаблоне остались фигурные скобки, которых сервис не понимает%s: %s. "+
		"Он знает одну подстановку - %s; всё остальное, включая двойные скобки и незакрытую скобку, уйдёт в письмо как есть, "+
		"поэтому уберите лишние скобки или закройте подстановку",
		shown, strings.Join(quoted, ", "), namePlaceholder)
}

// longTemplateError называет обе цифры: без них «сократите» не говорит,
// насколько резать.
func longTemplateError(err error) error {
	var long *longTemplate
	if !errors.As(err, &long) {
		return fmt.Errorf("шаблон не влезает в сообщение Telegram: предел %d символов", maxTextLen)
	}
	if long.budget == 0 {
		return fmt.Errorf("шаблон не влезает в сообщение Telegram: занято %d символов из %d. Лишних символов: %d",
			long.used, long.limit, long.used-long.limit)
	}
	return fmt.Errorf("шаблон не влезает в сообщение Telegram: занято %d символов из %d, из них %d - грубый запас на имя "+
		"(по %d на каждую подстановку; настоящее имя короче или длиннее, его длину сервис проверит на импорте). "+
		"Лишних символов: %d",
		long.used, long.limit, long.budget, nameBudget, long.used-long.limit)
}

// noDraftError различает пустую систему и живую кампанию рядом. Совет «создайте
// черновик» верен в обоих случаях, но при идущей кампании он звучит как «всё
// пропало»: агент обязан прочитать, что кампания цела, а список пополняет не
// она.
func noDraftError(err error) error {
	var near *nearCampaign
	if !errors.As(err, &near) {
		return errors.New("черновика нет: создайте его через load_campaign, потом добавляйте получателей")
	}
	return fmt.Errorf("черновика нет, а получателей принимает только он. Рядом кампания %d %q (%s), её список уже не пополнить: "+
		"load_campaign заведёт черновик рядом с ней, саму кампанию он не тронет",
		near.id, near.title, near.status)
}

// nothingToStopError отвечает по тому, что стоит рядом: повтор stop на уже
// приостановленной кампании иначе читается как «кампании больше нет», хотя её
// продолжает start.
func nothingToStopError(err error) error {
	var near *nearCampaign
	if !errors.As(err, &near) {
		return errors.New("останавливать нечего: идущей кампании нет")
	}
	if near.status != "stopped" {
		return fmt.Errorf("останавливать нечего: идущей кампании нет, рядом только черновик %d %q - "+
			"его снимает stop с cancel", near.id, near.title)
	}
	// Совет обязан считаться с черновиком рядом: start берёт черновик раньше
	// паузы, cancel бьёт по нему раньше паузы, и «продолжите start» запустило бы
	// не ту кампанию, а «снимите cancel» снесло бы собранный список.
	if near.draft != 0 {
		return fmt.Errorf("останавливать нечего: идущей кампании нет, кампания %d %q стоит на паузе. "+
			"Но рядом черновик %d, и обе команды пойдут по нему: start запустит черновик, stop с cancel снимет черновик. "+
			"До кампании %d очередь дойдёт не раньше, чем запущенная кампания будет остановлена: start при идущей кампании отказывает",
			near.id, near.title, near.draft, near.id)
	}
	// Соседние паузы тоже меняют совет: при нескольких stopped без черновика
	// start не выбирает сам и отказывает - обещать «продолжит start» нельзя.
	if near.pauses > 1 {
		return fmt.Errorf("останавливать нечего: идущей кампании нет, а на паузе кампаний несколько (%d), первая - %d %q. "+
			"start при нескольких паузах отказывается выбирать: снимите лишние через stop с cancel, тогда start продолжит оставшуюся",
			near.pauses, near.id, near.title)
	}
	return fmt.Errorf("останавливать нечего: идущей кампании нет, а кампания %d %q уже на паузе - "+
		"её продолжит start, а снимет вместе с очередью stop с cancel",
		near.id, near.title)
}

// campaignHoldError называет состояние кампании, которая держит человека, и
// действие, которое по нему сработает. Состояний три, а не два. Идущая кампания
// разберётся сама, но только пока движется очередь: вне окна отправки строка не
// сдвинется до ближайшего буднего утра, и «повторите через несколько минут» бьёт
// агента в стену. Кампания на паузе не разберётся никогда, и cancel бьёт по
// кампаниям в порядке active, draft, stopped - при живой соседней он снимет её,
// а не эту. Закрытую кампанию (отменена или завершена) сюда приводит только
// живой claim: строку прямо сейчас несёт отправщик, и действия по ней нет вовсе.
//
// Про «зависший claim в status» здесь не говорится: у возвращённой в очередь
// строки claimed_at пуст, и этот счётчик её не видит - совет отправлял бы агента
// за несуществующим признаком.
func campaignHoldError(err error) error {
	var hold *campaignHold
	if !errors.As(err, &hold) {
		return errors.New("этому человеку пишет кампания: повторите позже, иначе он получит два сообщения")
	}
	switch hold.status {
	case "active":
		// Идущая кампания со сломанным шаблоном очередь не двигает вовсе, и совет
		// «повторите через несколько минут» отправлял бы агента на второй круг.
		if hold.broken {
			return fmt.Errorf("человека держит строка кампании %d %q, но её очередь стоит: шаблон кампании не разобран. "+
				"Сама она не разберётся - снимите кампанию через stop с cancel, тогда человек освободится",
				hold.id, hold.title)
		}
		if hold.queueOpensAt.IsZero() {
			return fmt.Errorf("этому человеку прямо сейчас пишет кампания %d %q: очередь закроет строку сама, повторите через несколько минут",
				hold.id, hold.title)
		}
		return fmt.Errorf("человека держит строка идущей кампании %d %q, но очередь стоит вне окна отправки: "+
			"строка сдвинется не раньше %s МСК, до тех пор повторять бесполезно",
			hold.id, hold.title, hold.queueOpensAt.In(moscowZone).Format("02.01 15:04"))
	case "stopped":
		// Кампанию со сломанным шаблоном start не продолжает - тот же рубеж, что
		// стоит у идущей, - и совет «продолжите через start» упёрся бы в отказ.
		// Выход остаётся один: снять её вместе с очередью.
		if hold.broken {
			return fmt.Errorf("человека держит строка кампании %d %q на паузе, а шаблон этой кампании не разобран: "+
				"start её не продолжит, и сама она не разберётся - снимите её вместе с очередью через stop с cancel; "+
				"cancel бьёт по кампаниям в порядке active, draft, stopped, поэтому при живой активной кампании или черновике зовите его повторно, пока очередь не дойдёт до кампании %d",
				hold.id, hold.title, hold.id)
		}
		return fmt.Errorf("человека держит строка кампании %d %q на паузе, и сама она не разберётся: "+
			"продолжите кампанию через start либо снимите её вместе с очередью через stop с cancel - "+
			"cancel бьёт по кампаниям в порядке active, draft, stopped, поэтому при живой активной кампании или черновике зовите его повторно, пока очередь не дойдёт до кампании %d",
			hold.id, hold.title, hold.id)
	default:
		return fmt.Errorf("человека держит строка кампании %d %q, она уже закрыта (%s), но строку прямо сейчас несёт отправщик: "+
			"действия по ней нет - он закроет её сам, а брошенный claim протухает не дольше чем за %d минут",
			hold.id, hold.title, hold.status, minutesUp(hold.staleAfter))
	}
}

// accountStopError следует причине стопа. Ограничение Telegram снимает человек
// через resume_account после проверки аккаунта, а отозванную сессию тот же совет
// водит по кругу: стоп снят, демон получил ту же ошибку, стоп встал обратно.
// Выход из отозванной сессии один - повторный вход подкомандой authorize.
func accountStopError(err error) error {
	var stop *accountStop
	if !errors.As(err, &stop) {
		return errors.New("аккаунт остановлен: проверьте его руками, потом снимите стоп через resume_account")
	}
	if stop.reason == telegram.KindAuthForbidden {
		return fmt.Errorf("аккаунт остановлен: сессия Telegram больше не авторизована (%s). "+
			"resume_account тут не поможет - стоп встанет снова; войдите заново подкомандой authorize (docs/first-launch.md), потом снимите стоп",
			stop.reason)
	}
	return fmt.Errorf("аккаунт остановлен ограничением Telegram (%s): проверьте его руками, потом снимите стоп через resume_account",
		stop.reason)
}

// waitMinutes - сколько минут ещё считать доставку прошлого касания
// неизвестной. Срок задан настройкой сервиса, поэтому едет внутри ошибки.
func waitMinutes(err error) int {
	var pending *pendingWait
	if !errors.As(err, &pending) {
		return 1
	}
	return minutesUp(pending.left)
}

// minutesUp - срок в минутах с округлением вверх: совет «подождите 0 минут»
// разрешает повтор сразу.
func minutesUp(left time.Duration) int {
	value := int((left + time.Minute - time.Nanosecond) / time.Minute)
	if value < 1 {
		return 1
	}
	return value
}

// telegramError переводит отказ Telegram в решение вызывающего. Код MTProto
// идёт в текст: без него агент видит «временная ошибка Telegram», из чего не
// следует ни повтора, ни отказа.
func telegramError(failure *telegram.Failure) error {
	switch failure.Kind {
	case telegram.KindNotReady:
		return withCode("сессия Telegram ещё не поднята, повторите через несколько секунд; если держится - смотрите make live-logs", failure)
	case telegram.KindPeerForbidden:
		return withCode("личка закрыта, ник не существует или нас заблокировали", failure)
	case telegram.KindNotPerson:
		return withCode("ник занят группой или каналом: писать сервис умеет только человеку. Список диалогов их показывает, но отправка туда закрыта рубежом", failure)
	case telegram.KindFloodWait:
		// Длинный FLOOD_WAIT сюда не доходит: он останавливает аккаунт, и tg_send
		// отвечает стопом. Здесь остаётся короткая пауза, которую и правда можно
		// переждать, - об этом сказано прямо, иначе агент ждёт, повторяет и
		// получает «аккаунт остановлен».
		if failure.RetryAt.IsZero() {
			return withCode("Telegram просит переждать и повторить, аккаунт не остановлен", failure)
		}
		return withCode(fmt.Sprintf("Telegram просит переждать до %s МСК и повторить, аккаунт не остановлен",
			failure.RetryAt.In(moscowZone).Format("15:04")), failure)
	case telegram.KindPeerFlood:
		return withCode("аккаунт ограничен в письмах незнакомым: отправка с него остановлена и снимается только resume_account после проверки человеком", failure)
	case telegram.KindBadRequest:
		return withCode("Telegram отверг сам запрос: повтор того же даст тот же отказ. "+
			"Сообщение не создано - поправьте текст (слишком длинный или пустой) либо запрос поиска и позовите инструмент заново", failure)
	default:
		return failure
	}
}

func withCode(text string, failure *telegram.Failure) error {
	if failure.Type == "" {
		return errors.New(text)
	}
	return fmt.Errorf("%s (Telegram ответил %s)", text, failure.Type)
}

func textResult(text string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
}

// stateOrder задаёт порядок счётчиков в ответе: по map порядок случайный, и
// два вызова подряд выглядели бы как разные отчёты.
var stateOrder = []string{"planned", "sent", "replied", "undelivered", "skipped", "cancelled"}

var stateLabels = map[string]string{
	"planned":     "в очереди",
	"sent":        "отправлено",
	"replied":     "ответили",
	"undelivered": "не доставлено",
	"skipped":     "снято рубежом",
	"cancelled":   "отменено",
}

var reasonLabels = map[string]string{
	ReasonBadContact:   "нет id контакта amoCRM",
	ReasonNoUsername:   "нет пригодного ника Telegram",
	ReasonNoName:       "нет имени (кампании до 07.09.2026, сейчас письмо уходит без обращения)",
	ReasonWroteFirst:   "написал сам до того, как до него дошла очередь",
	ReasonTextTooLong:  "письмо с его именем длиннее предела Telegram",
	ReasonNameBraces:   "в имени фигурная скобка, она уедет в письмо",
	ReasonDuplicate:    "дубль: уже в кампании или дважды в пачке",
	ReasonStopList:     "стоп-лист",
	ReasonAlreadySent:  "уже получал сообщение",
	ReasonCampaignGone: "кампания отменена",
	ReasonNotAllowed:   "вне dev allow-list",
	// Две причины вместо одной: отчёт не имеет права утверждать доставку там,
	// где сервис её не знает.
	ReasonDirectSent:    "уже писали руками через tg_send",
	ReasonDirectPending: "писали руками, доставка неизвестна",
}

func label(labels map[string]string, key string) string {
	if text, ok := labels[key]; ok {
		return text
	}
	return key
}

func writeSkipped(text *strings.Builder, skipped map[string]int) {
	reasons := make([]string, 0, len(skipped))
	for reason, count := range skipped {
		if count > 0 {
			reasons = append(reasons, reason)
		}
	}
	if len(reasons) == 0 {
		return
	}
	sort.Strings(reasons)
	text.WriteString("Отсев:\n")
	for _, reason := range reasons {
		fmt.Fprintf(text, "  %s: %d\n", label(reasonLabels, reason), skipped[reason])
	}
}

func writeCampaign(text *strings.Builder, status StatusReport) {
	if status.Campaign == nil {
		text.WriteString("Кампаний нет: начните с load_campaign.\n")
		return
	}
	fmt.Fprintf(text, "Кампания: %s (id %d), статус %s.\n",
		status.Campaign.Title, status.Campaign.ID, status.Campaign.Status)
	fmt.Fprintf(text, "Строк всего: %d", status.Campaign.Total)
	for _, state := range stateOrder {
		if count := status.Campaign.States[state]; count > 0 {
			fmt.Fprintf(text, ", %s %d", label(stateLabels, state), count)
		}
	}
	text.WriteString("\n")
	// Сломанный шаблон - причина простоя, а не состояние кампании: статус её
	// остаётся тем, каким его оставил человек, поэтому очередь стоит молча, и
	// сказать об этом обязана именно эта строка.
	if len(status.Idle.Braces) > 0 {
		fmt.Fprintf(text, "Шаблон этой кампании не разобран (%q): отправки по ней не будет. "+
			"Соберите кампанию заново через load_campaign и add_recipients",
			status.Idle.Braces[0])
		// Снимать через stop советуется только живой кампании: по закрытой stop
		// откажет - снимать нечего, и совет отправил бы человека за отказом.
		switch status.Campaign.Status {
		case "active", "draft", "stopped":
			text.WriteString(", негодную снимите через stop с cancel")
		}
		text.WriteString(".\n")
	}
	// Строка, однажды взятая аккаунтом, остаётся за ним навсегда, поэтому при
	// нескольких отправщиках закреплённые строки - обычный ход рассылки, а не
	// сигнал. Тревожен только случай, когда держит их аккаунт, который сейчас
	// не отправляет: раньше других он и не встречался.
	for _, held := range status.Held {
		if heldStalled(status, held.Label) {
			fmt.Fprintf(text, "Закреплено за аккаунтом %s: %d строк - их возьмёт только он, а он сейчас не отправляет.\n",
				held.Label, held.Rows)
			continue
		}
		fmt.Fprintf(text, "Закреплено за аккаунтом %s: %d строк - он их и отправит своим чередом.\n",
			held.Label, held.Rows)
	}
	if status.Draft != nil {
		fmt.Fprintf(text, "Есть черновик (id %d): %d строк, он не запущен.\n",
			status.Draft.ID, status.Draft.Rows)
	}
	if status.Paused != nil {
		fmt.Fprintf(text, "На паузе кампания (id %d): %d строк, она возобновима.\n",
			status.Paused.ID, status.Paused.Rows)
	}
}

// writeIdle называет простой очереди словами: кампания при этом active, строки
// стоят planned, и такая картина неотличима от исправной работы - причину видно
// только косвенно, по счётчику расхода или по границам окна. Числа идут в самой
// строке: «потолок израсходован» без «7 из 7» проверить нечем.
func writeIdle(text *strings.Builder, status StatusReport) {
	body := idleBody(status.Idle, status)
	if body == "" {
		return
	}
	fmt.Fprintf(text, "%s: %s\n", idleLead(status.Idle.Reason), body)
}

// heldStalled - держит ли закреплённые строки аккаунт, который сейчас не
// отправляет: его нет среди аккаунтов вовсе либо он остановлен. Живой сосед с
// теми же строками - штатная картина рассылки с нескольких аккаунтов.
func heldStalled(status StatusReport, label string) bool {
	for _, item := range status.Accounts {
		if item.Label == label {
			return item.Stopped
		}
	}
	return true
}

// idleLead - чем открывается строка простоя: пустая очередь и занятый отправщик
// не «стоят», и одно слово на все причины врало бы в двух случаях из девяти.
func idleLead(reason string) string {
	switch reason {
	case IdleQueueEmpty:
		return "Очередь пуста"
	case IdleClaimRaceLost:
		return "Очередь движется"
	default:
		return "Очередь стоит"
	}
}

// idleBody - причина простоя словами. Одна формулировка на два места: общую
// строку отчёта и строку аккаунта, которая нужна там, где отправщиков несколько
// и молчит только один из них. Пустая строка означает, что причина уже названа
// рядом и повторять её нечем.
func idleBody(idle Idle, status StatusReport) string {
	switch idle.Reason {
	case IdleWindowClosed:
		if idle.OpensAt.IsZero() {
			return "окно отправки закрыто."
		}
		return fmt.Sprintf("окно отправки закрыто, ближайшее открытие %s МСК.",
			idle.OpensAt.In(moscowZone).Format("02.01 15:04"))
	case IdleAccountStop:
		return "аккаунт остановлен, снимает стоп только resume_account после проверки человеком."
	case IdleDailyLimit:
		return fmt.Sprintf("дневной потолок аккаунта израсходован (%d из %d), отправка продолжится завтра в окно.",
			idle.Used, idle.Limit)
	case IdleNoCampaign:
		// Совет звать start даётся только там, где есть что запускать: в пустой
		// системе строка выше уже сказала «кампаний нет», а по закрытой кампании
		// start откажет. Заметок Draft и Paused одних не хватает: StatusOf
		// заполняет их только когда показана не та же кампания, и единственный
		// черновик остался бы без совета.
		shown := status.Campaign != nil &&
			(status.Campaign.Status == "draft" || status.Campaign.Status == "stopped")
		switch {
		case shown || status.Draft != nil || status.Paused != nil:
			return "идущей кампании нет - запустите её через start."
		case status.Campaign != nil:
			return "кампания закрыта, новую создаёт load_campaign."
		}
		return ""
	case IdleBrokenTemplate:
		// Скобки показанной кампании уже названы строкой выше вместе с советом;
		// эта строка нужна там, где отчёт идёт по другой кампании.
		if len(status.Idle.Braces) == 0 {
			return "шаблон идущей кампании не разобран, отправки по ней не будет."
		}
		return ""
	case IdleQueueEmpty:
		return "строк в работе не осталось."
	case IdleQueueBlocked:
		return "оставшиеся строки закреплены за другим аккаунтом, их возьмёт только он."
	case IdleQueueDisqualified:
		return fmt.Sprintf("все оставшиеся строки (%d) снимает рубеж - стоп-лист, уже писали или негодное имя. "+
			"Отправок по этой кампании больше не будет, ближайший виток закроет её; разбор по строкам покажет status с full.",
			idle.Left)
	case IdleClaimRaceLost:
		return "доступные строки есть, отправщик берёт их по одной с паузами."
	}
	return ""
}

func writePreview(text *strings.Builder, status StatusReport) {
	if len(status.Preview) == 0 || status.Campaign == nil {
		return
	}
	// Превью упирается в потолок показа: без числа строк кампании три строки
	// читаются как весь список. Там, где среза нет, оговорки тоже нет - «первые 2
	// из 2» заставляет искать несуществующий остаток.
	if len(status.Preview) < status.Campaign.Total {
		fmt.Fprintf(text, "Превью сообщений (первые %d из %d):\n", len(status.Preview), status.Campaign.Total)
	} else {
		fmt.Fprintf(text, "Превью сообщений (%d):\n", len(status.Preview))
	}
	// Текст сокращается тем же приёмом, что и реплика в списке диалогов: превью
	// показывает подстановку, а не пересказывает письмо целиком.
	for _, item := range status.Preview {
		fmt.Fprintf(text, "  @%s: %s\n", item.Username, shorten(item.Text))
	}
}

// writeAccounts печатает расход аккаунта тем же числом, каким его считает рубеж:
// UsedToday собран одним usedTodaySQL и уже включает первые касания руками, а
// они названы отдельной строкой - человеку нужно видеть, чем израсходован
// потолок. Тёплые реплики и служебные адресаты в потолок не входят и стоят
// особняком, иначе строка отчёта обещала бы запас там, где tg_send уже
// отказывает.
func writeAccounts(text *strings.Builder, status StatusReport) {
	if len(status.Accounts) > 0 {
		text.WriteString("Аккаунты:\n")
		for _, item := range status.Accounts {
			fmt.Fprintf(text, "  %s: сегодня %d из %d (потолок из БД, с учётом строк в работе",
				item.Label, item.UsedToday, item.DailyLimit)
			if item.ColdToday > 0 {
				fmt.Fprintf(text, ", из них первых касаний руками: %d", item.ColdToday)
			}
			text.WriteString(")")
			if item.WarmToday > 0 {
				fmt.Fprintf(text, ", тёплых ответов сегодня: %d (в потолок не входят)", item.WarmToday)
			}
			if item.OwnerToday > 0 {
				fmt.Fprintf(text, ", служебных адресатов сегодня: %d (в потолок не входят)", item.OwnerToday)
			}
			if item.Stopped {
				fmt.Fprintf(text, ", ОСТАНОВЛЕН (%s), снимается только resume_account", item.StopReason)
			}
			// Причина молчания идёт в строку аккаунта только тогда, когда общей
			// строки нет: отправщиков несколько и молчат они по разным причинам.
			// У остановленного её не печатаем - о стопе сказано здесь же.
			if status.Idle.Reason == "" && item.Idle.Reason != IdleAccountStop {
				if body := idleBody(item.Idle, status); body != "" {
					fmt.Fprintf(text, ", молчит: %s", strings.TrimSuffix(body, "."))
				}
			}
			text.WriteString("\n")
		}
	}
	// Список служебных адресатов называется поимённо и всегда, а не только когда
	// им сегодня писали: для этих ников выключены окно, потолок и правило «одно
	// сообщение», и лишний ник в нём иначе обнаружился бы только ушедшим
	// сообщением.
	if len(status.Owners) > 0 {
		text.WriteString("Служебные адресаты (окно, потолок и правило «одно сообщение» на них не действуют):")
		for _, name := range status.Owners {
			fmt.Fprintf(text, " @%s", name)
		}
		text.WriteString("\n")
	}
	// Зависшая строка журнала закрывает человека навсегда, а доставка
	// неизвестна: чинится руками, поэтому обязана попадаться на глаза.
	if status.StuckDirect > 0 {
		fmt.Fprintf(text, "Ручных касаний с неизвестной доставкой: %d\n", status.StuckDirect)
	}
	// Строка с протухшим claim у кампании на паузе сама не чинится: она держит
	// человека закрытым и для рассылки, и для tg_send.
	if status.StuckClaims > 0 {
		fmt.Fprintf(text, "Строк с зависшим claim: %d\n", status.StuckClaims)
	}
}

// writeContacts печатает находки поиска. needle - запрос в том же виде, в каком
// его искал Telegram: без @, без пробелов по краям и в нижнем регистре.
func writeContacts(text *strings.Builder, needle string, found []Contact) {
	byUsername := telegram.IsUsername(needle)
	if len(found) == 0 {
		if byUsername {
			fmt.Fprintf(text, "Ника @%s нет и похожих не нашлось.\nДальше: проверьте ник на опечатку либо поищите человека по имени.\n", needle)
			return
		}
		fmt.Fprintf(text, "По запросу %q никого не нашлось.\nДальше: назовите точный ник без @ либо поищите человека по другому написанию имени.\n", needle)
		return
	}

	// Точный ник переставляется в голову списка: в префиксной выдаче тот, кого
	// искали, стоит вперемешку с чужими похожими никами и теряется. Находка одна
	// - ник в Telegram уникален, - поэтому порядок остальных не меняется.
	list := found
	exact := -1
	for i, item := range found {
		if item.Exact {
			exact = i
			break
		}
	}
	if exact > 0 {
		list = make([]Contact, 0, len(found))
		list = append(list, found[exact])
		list = append(list, found[:exact]...)
		list = append(list, found[exact+1:]...)
	}

	fmt.Fprintf(text, "Найдено по запросу %q: %d\n", needle, len(list))
	// Про список, а не про Telegram: при полном лимите похожих точный ник не
	// проверялся вовсе, и «в Telegram такого ника нет» было бы обещанием, за
	// которое отчёт не отвечает.
	if byUsername && exact < 0 {
		fmt.Fprintf(text, "Среди найденных точного ника @%s нет: ниже похожие ники, это другие люди. "+
			"tg_send по ним - только после проверки, что ник взят не с опечаткой.\n", needle)
	}
	for _, item := range list {
		fmt.Fprintf(text, "  %s", personHead(item.Username, item.Name))
		if item.Exact {
			text.WriteString(" - точный ник")
		}
		// Про диалог строка молчит намеренно: сервис знает только, встречался ли
		// адрес раньше, а туда же кладёт ник обычный резолв под tg_read. Обещав
		// диалог, отчёт заставил бы планировать ответ как тёплый и получить
		// отказ по окну.
		// Адресная книга сильнее «адрес встречался»: человек из контактов почти
		// всегда знакомый, и обратный порядок веток не давал бы этой строке
		// появиться вовсе - у локальной находки Known стоит всегда.
		switch {
		case item.IsContact:
			text.WriteString(" - в контактах")
		case item.Known:
			text.WriteString(" - ник уже встречался")
		default:
			text.WriteString(" - сервису незнаком")
		}
		if item.Username == "" {
			text.WriteString(noUsernameNote)
		}
		text.WriteString("\n")
	}
	text.WriteString("Дальше: tg_read покажет диалог, tg_send напишет. " +
		"Тёплая это переписка или первое касание, решает отправка: первое касание уходит только в окно и в потолок.\n")
}

// personHead - человек в начале строки списка. Ник по имени не угадывается
// (инвариант 5), поэтому у человека без публичного ника печатается имя, а не
// пустой @: пустой @ агент принимает за ник и подставляет его в tg_send.
func personHead(username, name string) string {
	switch {
	case username != "" && name != "":
		return "@" + username + " " + name
	case username != "":
		return "@" + username
	case name != "":
		return name
	default:
		return "без имени и ника"
	}
}

// noUsernameNote называет отсутствие ника прямо: рубеж tg_send такого человека
// всё равно остановит, но разбор отказа обойдётся дороже, чем эта строка.
const noUsernameNote = "; публичного ника нет, tg_send ему не напишет"

// writeConversation печатает диалог так, как его читает человек: от старых к
// новым, чужое и своё названы словом, текст не обрезается - объём держит
// depth. Имени собеседника и полного числа сообщений в заголовке нет: сервис
// их не знает, а лишний запрос ради шапки не ставится.
func writeConversation(text *strings.Builder, talk Conversation) {
	fmt.Fprintf(text, "Диалог с @%s", talk.Username)
	if talk.UserID > 0 {
		fmt.Fprintf(text, " (id %d)", talk.UserID)
	}
	if len(talk.Messages) == 0 {
		text.WriteString(": сообщений нет.\n")
	} else {
		fmt.Fprintf(text, ", последние сообщения (%d):\n", len(talk.Messages))
		writeMessages(text, talk.Messages, false)
	}

	switch {
	case len(talk.Fresh) > 0:
		fmt.Fprintf(text, "Новое сообщение через %d секунд:\n", int(talk.Waited.Seconds()))
		writeMessages(text, talk.Fresh, false)
	case talk.Waited > 0:
		fmt.Fprintf(text, "Ждали %d секунд, реплики не было. Нужно ждать дальше - позовите tg_read ещё раз: "+
			"ожидание продолжится с этого места.\n", int(talk.Waited.Seconds()))
	default:
		text.WriteString("Не ждали: без wait_seconds инструмент отвечает сразу.\n")
	}
	text.WriteString("Дальше: tg_send ответит в этот диалог, tg_read с wait_seconds подождёт реплику.\n")
}

// dialogKind называет вид строки по смыслу действия, а не по устройству
// Telegram: супергруппа и обычная группа читаются одинаково - место, где
// переписываются люди, - и обе называются группой; вещательный канал остаётся
// каналом.
func dialogKind(kind telegram.Kind, broadcast bool) string {
	switch {
	case kind == telegram.KindUser:
		return "человек"
	case kind == telegram.KindChannel && broadcast:
		return "канал"
	case kind == telegram.KindChannel || kind == telegram.KindChat:
		return "группа"
	default:
		return "неизвестно"
	}
}

func writeDialogs(text *strings.Builder, list []DialogInfo) {
	if len(list) == 0 {
		// Отсева групп и каналов больше нет: список пуст, только если у аккаунта
		// нет вовсе ни одного диалога.
		text.WriteString("Диалогов нет ни одного - ни личных, ни групп, ни каналов.\nДальше: tg_find найдёт человека по нику, tg_send начнёт диалог.\n")
		return
	}
	fmt.Fprintf(text, "Диалоги (%d):\n", len(list))
	for _, item := range list {
		kind, _, err := item.Ref.Split()
		if err != nil {
			// Испорченная ссылка не прячет строку: вид будет назван «неизвестно».
			kind = ""
		}
		person := kind == telegram.KindUser
		head := personHead(item.Username, item.Name)
		// Пустое имя у группы и канала значит, что сущности не было в ответе
		// (аккаунт исключён), а не человека без ника: безымянных групп не бывает.
		if !person && head == "без имени и ника" {
			head = "название неизвестно"
		}
		fmt.Fprintf(text, "  %s: %s", dialogKind(kind, item.Broadcast), head)
		if item.Unread > 0 {
			fmt.Fprintf(text, ", непрочитанных %d", item.Unread)
		}
		// Пустой ник у группы и супергруппы - устройство платформы, не пропажа:
		// предупреждение про tg_send печатается только человеку.
		if person && item.Username == "" {
			text.WriteString(noUsernameNote)
		}
		text.WriteString("\n")
		// Верхнее сообщение группы бывает служебным, и тогда его нет вовсе:
		// строка «он: [без текста]» на пустом месте читается как потерянный текст.
		if item.Last != (ChatMessage{}) {
			writeMessages(text, []ChatMessage{item.Last}, true)
		}
	}
	text.WriteString("Дальше: tg_read покажет диалог целиком. Написать можно только человеку - tg_send группе и каналу откажет рубежом.\n")
}

// snippetLen - потолок реплики в списке диалогов: пост канала на двадцать
// строк вытесняет с экрана сам список, ради которого инструмент и звали.
const snippetLen = 100

// writeMessages - строки диалога: время в МСК, направление словом, вложение
// пометкой перед текстом. short схлопывает реплику в одну строку - так её
// печатает список диалогов; чтение диалога отдаёт текст целиком, объём там
// держит depth. Время у сообщения без даты (журнал dry-run) не печатается
// вовсе: подставленная дата выглядела бы как настоящая.
func writeMessages(text *strings.Builder, messages []ChatMessage, short bool) {
	for _, msg := range messages {
		text.WriteString("  ")
		if !msg.SentAt.IsZero() {
			fmt.Fprintf(text, "[%s] ", msg.SentAt.In(moscowZone).Format("02.01 15:04"))
		}
		if msg.Outgoing {
			text.WriteString("мы: ")
		} else {
			text.WriteString("он: ")
		}
		body := msg.Text
		if short {
			body = shorten(body)
		}
		// Пустое сообщение без вложения (служебное или удалённый текст) читается
		// как сломанная строка, поэтому названо словом.
		switch {
		case msg.MediaKind != "" && body != "":
			fmt.Fprintf(text, "[%s] %s", msg.MediaKind, body)
		case msg.MediaKind != "":
			fmt.Fprintf(text, "[%s]", msg.MediaKind)
		case body != "":
			text.WriteString(body)
		default:
			text.WriteString("[без текста]")
		}
		text.WriteString("\n")
	}
}

// shorten схлопывает реплику в одну строку и режет её по рунам: срез по байтам
// порвал бы кириллицу или эмодзи посередине.
func shorten(body string) string {
	return cutRunes(strings.Join(strings.Fields(body), " "), snippetLen)
}

// templateShown - сколько символов шаблона печатает load_campaign. Письмо читают
// глазами целиком, поэтому потолок щедрый; режется только то, что вытеснило бы
// из ответа всё остальное.
const templateShown = 500

// cutRunes режет текст по рунам, не трогая переносы строк: шаблон проверяют как
// письмо, и схлопнутый в строку он для этого не годится.
func cutRunes(body string, limit int) string {
	runes := []rune(body)
	if len(runes) <= limit {
		return body
	}
	return string(runes[:limit]) + "..."
}

func writeRows(text *strings.Builder, status StatusReport) {
	fmt.Fprintf(text, "Получатели (%d):\n", len(status.Rows))
	for _, row := range status.Rows {
		fmt.Fprintf(text, "  @%s %s - %s", row.Username, row.Name, label(stateLabels, row.State))
		if row.Reason != "" {
			fmt.Fprintf(text, " (%s)", label(reasonLabels, row.Reason))
		}
		text.WriteString("\n")
	}
}
