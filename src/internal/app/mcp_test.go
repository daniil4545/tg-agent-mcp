package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/daniil4545/tg-agent-mcp/internal/telegram"
)

// toolText прогоняет обработчик инструмента и отдаёт текст ответа. Полный
// HTTP-транспорт здесь не нужен: проверяется решение инструмента, а не
// streamable-обвязка сервера.
func toolText(t *testing.T, result *mcp.CallToolResult, err error) string {
	t.Helper()
	if err != nil {
		t.Fatalf("инструмент отказал: %v", err)
	}
	var text strings.Builder
	for _, content := range result.Content {
		if item, ok := content.(*mcp.TextContent); ok {
			text.WriteString(item.Text)
		}
	}
	return text.String()
}

// Инструменты кампании id не принимают: add_recipients работает с единственным
// черновиком, а без черновика обязан внятно сказать, чего не хватает.
func TestAddRecipientsUsesSingleDraft(t *testing.T) {
	handlers := &tools{service: newService(t)}
	ctx := context.Background()

	if _, _, err := handlers.addRecipients(ctx, nil, addArgs{People: []personArgs{{ContactID: 1, Username: "ivan_petrov"}}}); err == nil {
		t.Fatal("пачка принята без черновика")
	} else if !strings.Contains(err.Error(), "load_campaign") {
		t.Fatalf("отказ без черновика не называет следующий шаг: %v", err)
	}

	if _, _, err := handlers.loadCampaign(ctx, nil, loadArgs{Template: "Привет, {имя}!", Title: "тест"}); err != nil {
		t.Fatalf("создать черновик: %v", err)
	}

	result, _, err := handlers.addRecipients(ctx, nil, addArgs{People: []personArgs{
		{ContactID: 1, Name: "Иван", Username: "ivan_petrov"},
		{ContactID: 2, Name: "Пётр", Username: "пётр"},
		{ContactID: 1, Name: "Иван", Username: "ivan_petrov"},
	}})
	text := toolText(t, result, err)

	if !strings.Contains(text, "Принято: 1 из 3") {
		t.Fatalf("отчёт не называет число принятых:\n%s", text)
	}
	if !strings.Contains(text, reasonLabels[ReasonNoUsername]) || !strings.Contains(text, reasonLabels[ReasonDuplicate]) {
		t.Fatalf("отчёт не называет причины отсева:\n%s", text)
	}
	if !strings.Contains(text, "Привет, Иван!") {
		t.Fatalf("в отчёте нет превью сообщения:\n%s", text)
	}
}

// Черновик рядом с идущей кампанией: add_recipients отчитывается по черновику,
// в который вставлял, а status - по активной, назвав черновик отдельной
// строкой. Иначе сверка списка перед стартом шла бы по чужой кампании.
func TestReportsFollowTheEditedCampaign(t *testing.T) {
	service := newService(t)
	handlers := &tools{service: service}
	ctx := context.Background()

	startWith(t, service, person(1, "active_one"))
	if _, _, err := handlers.loadCampaign(ctx, nil, loadArgs{Template: "Черновик, {имя}!", Title: "второй"}); err != nil {
		t.Fatalf("создать черновик: %v", err)
	}

	result, _, err := handlers.addRecipients(ctx, nil, addArgs{
		People: []personArgs{{ContactID: 2, Name: "Пётр", Username: "petr_ivanov"}},
	})
	added := toolText(t, result, err)
	if !strings.Contains(added, "второй") || !strings.Contains(added, "Черновик, Пётр!") {
		t.Fatalf("отчёт вставки не по черновику:\n%s", added)
	}
	if strings.Contains(added, "active_one") {
		t.Fatalf("в отчёте вставки превью чужой кампании:\n%s", added)
	}

	statusResult, _, err := handlers.status(ctx, nil, statusArgs{})
	shown := toolText(t, statusResult, err)
	if !strings.Contains(shown, "Привет, Иван!") {
		t.Fatalf("status показывает не активную кампанию:\n%s", shown)
	}
	if !strings.Contains(shown, "Есть черновик") {
		t.Fatalf("status не назвал черновик:\n%s", shown)
	}
}

// Повторный stop на уже приостановленной кампании: кампания жива, и отказ обязан
// назвать паузу и продолжение через start, а не советовать создать черновик.
func TestStopAgainNamesThePause(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	startWith(t, service, person(1, "first_one"))
	if _, err := service.StopCampaign(ctx, false); err != nil {
		t.Fatalf("пауза: %v", err)
	}

	handlers := &tools{service: service}
	_, _, err := handlers.stop(ctx, nil, stopArgs{})
	if err == nil {
		t.Fatal("повторная пауза принята как остановка")
	}
	if !strings.Contains(err.Error(), "уже на паузе") || !strings.Contains(err.Error(), "start") {
		t.Fatalf("отказ не назвал живую паузу: %v", err)
	}
}

// Тот же отказ при паре «пауза плюс черновик»: start возьмёт черновик, и cancel
// снимет черновик, поэтому совет «продолжите паузу через start» отправил бы
// агента запускать чужую кампанию, а «снимите cancel» - сносить собранный список.
func TestStopRefusalCountsTheDraft(t *testing.T) {
	service := newService(t)
	paused, draft := pausedAndDraft(t, service)

	handlers := &tools{service: service}
	_, _, err := handlers.stop(context.Background(), nil, stopArgs{})
	if err == nil {
		t.Fatal("повторная пауза принята как остановка")
	}
	text := err.Error()
	if !strings.Contains(text, fmt.Sprintf("черновик %d", draft)) {
		t.Fatalf("отказ не назвал черновик рядом с паузой %d: %v", paused, err)
	}
	if !strings.Contains(text, "start запустит черновик") || !strings.Contains(text, "cancel снимет черновик") {
		t.Fatalf("отказ обещает действия по паузе, а они пойдут по черновику: %v", err)
	}
}

// Простой очереди называется в status: кампания остаётся active, строки стоят в
// очереди, и без этой строки причина видна только косвенно. Проверяются те две
// причины, которых не хватило на живом тесте, - потолок и закрытое окно.
func TestStatusNamesIdleReason(t *testing.T) {
	service := newService(t)
	handlers := &tools{service: service}
	ctx := context.Background()

	startWith(t, service, person(1, "ivan_petrov"), person(2, "petr_ivanov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("финализация: %v", err)
	}
	runSQL(t, service, `UPDATE accounts SET daily_limit = 1 WHERE label = 'test'`)

	result, _, err := handlers.status(ctx, nil, statusArgs{})
	text := toolText(t, result, err)
	if !strings.Contains(text, "дневной потолок аккаунта израсходован (1 из 1)") {
		t.Fatalf("status не назвал простой по потолку:\n%s", text)
	}

	// Суббота той же недели: окно закрыто и перекрывает потолок, а открытие
	// названо часом, иначе «ждите окна» не говорит, до каких пор.
	service.Now = func() time.Time { return testNow.AddDate(0, 0, 3) }
	result, _, err = handlers.status(ctx, nil, statusArgs{})
	text = toolText(t, result, err)
	if !strings.Contains(text, "окно отправки закрыто, ближайшее открытие 07.09 10:00 МСК") {
		t.Fatalf("status не назвал простой по окну:\n%s", text)
	}
}

// Расход на служебных адресатов владельца называется в status отдельной
// строкой, тем же приёмом, что и тёплые ответы: в потолок он не входит, и без
// этой строки его не видно вовсе.
func TestStatusNamesOwnerSpending(t *testing.T) {
	service := newService(t, "smirnov")
	handlers := &tools{service: service}
	ctx := context.Background()

	startWith(t, service, person(1, "smirnov"))
	claimed, err := service.ClaimNext(ctx, "test")
	if err != nil || claimed == nil {
		t.Fatalf("взять строку служебного адресата: %v, %+v", err, claimed)
	}
	if _, err := service.FinishClaim(ctx, claimed, ResultSent, ""); err != nil {
		t.Fatalf("финализация: %v", err)
	}

	result, _, err := handlers.status(ctx, nil, statusArgs{})
	text := toolText(t, result, err)
	if !strings.Contains(text, "служебных адресатов сегодня: 1 (в потолок не входят)") {
		t.Fatalf("status не назвал расход на служебных адресатов:\n%s", text)
	}
	// Состав списка называется поимённо: снятый рубеж, о котором никто не узнал,
	// равен его отсутствию, а лишний ник в DEV_ALLOW_LIST виден только здесь.
	if !strings.Contains(text, "Служебные адресаты (окно, потолок и правило «одно сообщение» на них не действуют): @smirnov") {
		t.Fatalf("status не назвал состав служебных адресатов:\n%s", text)
	}
}

// resume_account отвечает по факту: снятый стоп назван причиной, живой аккаунт -
// прямо, незнакомая метка - отказом. Ответ «стоп снят» там, где стопа не было,
// отправляет человека чинить несуществующую поломку.
func TestResumeAccountAnswersByState(t *testing.T) {
	service := newService(t)
	handlers := &tools{service: service}
	ctx := context.Background()

	quiet, _, err := handlers.resumeAccount(ctx, nil, resumeArgs{Label: "test"})
	if text := toolText(t, quiet, err); !strings.Contains(text, `Стопа на аккаунте "test" нет: снимать нечего`) {
		t.Fatalf("живой аккаунт отвечает как снятый стоп:\n%s", text)
	}

	if err := service.StopAccount(ctx, "test", telegram.KindPeerFlood); err != nil {
		t.Fatalf("остановить аккаунт: %v", err)
	}
	result, _, err := handlers.resumeAccount(ctx, nil, resumeArgs{Label: "test"})
	text := toolText(t, result, err)
	if !strings.Contains(text, `Стоп с аккаунта "test" снят`) || !strings.Contains(text, telegram.KindPeerFlood) {
		t.Fatalf("снятие стопа не названо причиной:\n%s", text)
	}
	if stopped, _, err := service.AccountStopped(ctx, "test"); err != nil || stopped {
		t.Fatalf("стоп не снят: %v, %v", stopped, err)
	}

	if _, _, err := handlers.resumeAccount(ctx, nil, resumeArgs{Label: "unknown"}); err == nil {
		t.Fatal("метка без аккаунта принята как снятый стоп")
	}
}

// Превью показывает не больше трёх строк: заголовок обязан назвать, сколько их в
// кампании, иначе три строки читаются как весь список.
func TestPreviewNamesItsLimit(t *testing.T) {
	service := newService(t)
	handlers := &tools{service: service}

	startWith(t, service,
		person(1, "ivan_petrov"), person(2, "petr_ivanov"),
		person(3, "anna_smirnova"), person(4, "olga_orlova"))

	result, _, err := handlers.status(context.Background(), nil, statusArgs{})
	text := toolText(t, result, err)
	if !strings.Contains(text, "Превью сообщений (первые 3 из 4):") {
		t.Fatalf("превью не называет потолок показа:\n%s", text)
	}
}

// failDialog - транспорт, у которого чтение отказывает: отказ живого Telegram
// приходит на резолве, а stubDialog умеет ронять только отправку.
type failDialog struct {
	stubDialog
	err error
}

func (d *failDialog) Lookup(context.Context, string) (Peer, error) { return Peer{}, d.err }

// Регистрация - единственное, что отличает написанный обработчик от
// доступного агенту: инструмент вне списка сервера не существует.
func TestDialogToolsRegistered(t *testing.T) {
	ctx := context.Background()
	serverSide, clientSide := mcp.NewInMemoryTransports()

	server := NewMCPServer(&Service{cfg: Config{AccountLabel: "test"}, Now: time.Now}, nil)
	serverSession, err := server.Connect(ctx, serverSide, nil)
	if err != nil {
		t.Fatalf("поднять сервер: %v", err)
	}
	defer func() { _ = serverSession.Close() }()

	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatalf("подключиться: %v", err)
	}
	defer func() { _ = session.Close() }()

	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("список инструментов: %v", err)
	}
	registered := map[string]bool{}
	for _, tool := range list.Tools {
		registered[tool.Name] = true
	}
	for _, name := range []string{"tg_find", "tg_send", "tg_read", "tg_dialogs"} {
		if !registered[name] {
			t.Fatalf("инструмент %s не зарегистрирован: %v", name, registered)
		}
	}
}

// Аргументы status и stop необязательны, и схема обязана это говорить: поле без
// omitempty объявляется required, и вызов без аргументов отбивается проверкой
// схемы, не дойдя до обработчика. Живой status отвечал так на пустой объект.
// Проверка накрывает и campaign_id: у int64 та же болезнь, что у флага.
func TestOptionalFlagsAreNotRequired(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	serverSide, clientSide := mcp.NewInMemoryTransports()

	serverSession, err := NewMCPServer(service, nil).Connect(ctx, serverSide, nil)
	if err != nil {
		t.Fatalf("поднять сервер: %v", err)
	}
	defer func() { _ = serverSession.Close() }()

	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatalf("подключиться: %v", err)
	}
	defer func() { _ = session.Close() }()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "status"})
	if err != nil {
		t.Fatalf("status без аргументов отказал: %v", err)
	}
	if text := toolText(t, result, nil); result.IsError {
		t.Fatalf("status без аргументов отвечает отказом:\n%s", text)
	}

	// stop без кампании отказывает по делу; проверяется, что отказ доменный, а не
	// схемы: у флага cancel та же болезнь и то же лечение.
	stopped, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "stop"})
	if err != nil {
		t.Fatalf("stop без аргументов отказал: %v", err)
	}
	if text := toolText(t, stopped, nil); strings.Contains(text, "validating") {
		t.Fatalf("stop без аргументов отбит схемой:\n%s", text)
	}
}

// Срез по умолчанию - самый частый вызов, и молчит он опаснее явного: двадцать
// первых строк агент читает как весь список и делает вывод «человека нет».
func TestToolsNameTheirCut(t *testing.T) {
	ctx := context.Background()

	// Ровно defaultHistory штук: срез до limit теперь держит транспорт (проверено
	// в internal/telegram), а Chat.Dialogs отдаёт его выборку как есть, без
	// второго обрезания. Подставной транспорт своим limit не пользуется, поэтому
	// список собирается уже нужного размера - так же, как его отдал бы настоящий.
	crowd := make([]DialogInfo, 0, defaultHistory)
	for i := range defaultHistory {
		crowd = append(crowd, DialogInfo{
			Username: fmt.Sprintf("user_%02d", i),
			Ref:      telegram.NewRef(telegram.KindUser, int64(i+10)),
			Last:     ChatMessage{Text: "привет"},
		})
	}
	full := make([]ChatMessage, 0, defaultHistory)
	for i := range defaultHistory {
		full = append(full, ChatMessage{ID: i + 1, Text: "привет"})
	}
	handlers := liveTools(&stubDialog{dialogs: crowd, poll: func(int) []ChatMessage { return full }})

	result, _, err := handlers.dialogs(ctx, nil, dialogsArgs{})
	listed := toolText(t, result, err)
	if !strings.Contains(listed, "Показано 20 последних диалогов") || !strings.Contains(listed, "limit до 100") {
		t.Fatalf("список диалогов не назвал срез по умолчанию:\n%s", listed)
	}
	if !strings.Contains(listed, "Диалоги (20)") {
		t.Fatalf("срез по умолчанию не применён:\n%s", listed)
	}

	deep, _, err := handlers.read(ctx, nil, readArgs{Username: "ivan_petrov"})
	if shown := toolText(t, deep, err); !strings.Contains(shown, "Показано не всё") {
		t.Fatalf("чтение диалога не назвало исчерпанную глубину:\n%s", shown)
	}

	// Список короче предела показан целиком: и оговорка про срез, и совет звать
	// tg_dialogs с большим limit отправили бы агента за несуществующим остатком.
	few := liveTools(&stubDialog{dialogs: crowd[:3]})
	shortList, _, err := few.dialogs(ctx, nil, dialogsArgs{})
	if shown := toolText(t, shortList, err); strings.Contains(shown, "последних диалогов") || strings.Contains(shown, "limit до 100") {
		t.Fatalf("полный список диалогов назван срезом:\n%s", shown)
	}

	// Диалог короче глубины показан целиком: оговорка тут отправила бы агента за
	// несуществующим остатком.
	brief := liveTools(&stubDialog{poll: func(int) []ChatMessage { return full[:3] }})
	short, _, err := brief.read(ctx, nil, readArgs{Username: "ivan_petrov"})
	if shown := toolText(t, short, err); strings.Contains(shown, "Показано не всё") {
		t.Fatalf("полный диалог назван срезом:\n%s", shown)
	}
}

// Отказ рубежа доходит до агента человеческим текстом с названным следующим
// шагом: доменная ошибка на английском не говорит вызывающему ничего.
func TestBoundaryRefusalReadsAsText(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	if _, err := service.pool.Exec(ctx, `INSERT INTO stop_list (username) VALUES ('ivan_petrov')`); err != nil {
		t.Fatalf("стоп-лист: %v", err)
	}
	handlers := &tools{service: service, chat: chatOn(service, &stubDialog{})}

	_, _, err := handlers.send(ctx, nil, sendArgs{Username: "ivan_petrov", Text: "привет"})
	if err == nil {
		t.Fatal("сообщение человеку из стоп-листа принято")
	}
	if !strings.Contains(err.Error(), "стоп-лист") {
		t.Fatalf("отказ рубежа не переведён на человеческий: %v", err)
	}
}

// Неподнятая сессия - самый частый отказ живого контура: агент обязан
// прочитать, что делать, а не «временную ошибку Telegram».
func TestNotReadySessionExplainsItself(t *testing.T) {
	handlers := &tools{
		service: &Service{cfg: Config{AccountLabel: "test"}, Now: time.Now},
		chat: offlineChat(&failDialog{err: &telegram.Failure{
			Kind: telegram.KindNotReady, Message: "клиент не запущен",
		}}),
	}

	_, _, err := handlers.read(context.Background(), nil, readArgs{Username: "ivan_petrov"})
	if err == nil {
		t.Fatal("чтение без сессии прошло")
	}
	if !strings.Contains(err.Error(), "сессия") || !strings.Contains(err.Error(), "live-logs") {
		t.Fatalf("отказ не называет неподнятую сессию: %v", err)
	}
}

// tg_send отчитывается ценой, а не фактом отправки: после ручного касания
// человек закрыт для всех кампаний, и вызывающий обязан это прочитать.
func TestDirectSendAnswerNamesThePrice(t *testing.T) {
	service := newService(t)
	handlers := &tools{service: service, chat: chatOn(service, &stubDialog{})}

	result, _, err := handlers.send(context.Background(), nil, sendArgs{Username: "@Ivan_Petrov", Text: "привет"})
	text := toolText(t, result, err)

	if !strings.Contains(text, "Касание записано: этот человек больше не попадёт ни в одну кампанию.") {
		t.Fatalf("ответ tg_send не называет цену касания:\n%s", text)
	}
	if !strings.Contains(text, "@ivan_petrov") {
		t.Fatalf("ответ tg_send не называет нормализованный ник:\n%s", text)
	}
}

// liveTools - инструменты на живом контуре без базы: списки и чтение в неё не
// ходят, а метка dry-run сбила бы проверку текста ответа.
func liveTools(transport Dialog) *tools {
	return &tools{
		service: &Service{cfg: Config{AccountLabel: "test", Transport: "live"}, Now: time.Now},
		chat:    offlineChat(transport),
	}
}

// Человек без публичного ника: пустой @ агент принимает за ник и подставляет
// его в tg_send. Ответ обязан назвать имя и сказать, что написать нельзя.
func TestPeopleWithoutUsernameAreNamed(t *testing.T) {
	ctx := context.Background()
	handlers := liveTools(&stubDialog{
		found: []Contact{{Name: "Дмитрий стоматолог", IsContact: true}},
		dialogs: []DialogInfo{{
			Name: "Дмитрий стоматолог",
			Ref:  telegram.NewRef(telegram.KindUser, 10),
			Last: ChatMessage{Text: "привет"},
		}},
	})

	for _, run := range []struct {
		name string
		call func() (*mcp.CallToolResult, any, error)
	}{
		{"tg_find", func() (*mcp.CallToolResult, any, error) {
			return handlers.find(ctx, nil, findArgs{Query: "Дмитрий"})
		}},
		{"tg_dialogs", func() (*mcp.CallToolResult, any, error) {
			return handlers.dialogs(ctx, nil, dialogsArgs{})
		}},
	} {
		t.Run(run.name, func(t *testing.T) {
			result, _, err := run.call()
			text := toolText(t, result, err)
			if strings.Contains(text, "@") {
				t.Fatalf("человек без ника напечатан пустым @:\n%s", text)
			}
			if !strings.Contains(text, "Дмитрий стоматолог") || !strings.Contains(text, "публичного ника нет") {
				t.Fatalf("ответ не называет человека без ника:\n%s", text)
			}
		})
	}
}

// Список диалогов - обзор: длинная реплика схлопывается в одну строку и
// режется по рунам, а чтение диалога отдаёт тот же текст целиком.
func TestDialogListShortensLastMessage(t *testing.T) {
	ctx := context.Background()
	long := strings.Repeat("я", 150) + "\nвторая строка"
	handlers := liveTools(&stubDialog{
		dialogs: []DialogInfo{{
			Username: "ivan_petrov",
			Ref:      telegram.NewRef(telegram.KindUser, 10),
			Last:     ChatMessage{Text: long},
		}},
		poll: func(int) []ChatMessage { return []ChatMessage{{ID: 1, Text: long}} },
	})

	result, _, err := handlers.dialogs(ctx, nil, dialogsArgs{})
	listed := toolText(t, result, err)
	if !strings.Contains(listed, strings.Repeat("я", snippetLen)+"...") {
		t.Fatalf("реплика в списке не обрезана по рунам:\n%s", listed)
	}
	if strings.Contains(listed, "вторая строка") {
		t.Fatalf("реплика в списке заняла больше одной строки:\n%s", listed)
	}

	readResult, _, err := handlers.read(ctx, nil, readArgs{Username: "ivan_petrov"})
	shown := toolText(t, readResult, err)
	if !strings.Contains(shown, long) {
		t.Fatalf("чтение диалога обрезало текст:\n%s", shown)
	}
}

// Список смешан по видам: агент решает по слову в строке, куда можно писать, а
// куда только читать. Безымянная сущность - группа или канал, откуда аккаунт
// исключён; служебное верхнее сообщение приходит пустым и строки не печатает.
func TestDialogListNamesEveryKind(t *testing.T) {
	handlers := liveTools(&stubDialog{dialogs: []DialogInfo{
		{Username: "ivan_petrov", Name: "Иван", Ref: telegram.NewRef(telegram.KindUser, 10), Unread: 2,
			Last: ChatMessage{Text: "привет"}},
		{Name: "Дмитрий стоматолог", Ref: telegram.NewRef(telegram.KindUser, 11)},
		{Name: "Чат клуба", Ref: telegram.NewRef(telegram.KindChat, 12), Unread: 7,
			Last: ChatMessage{Text: "обсуждаем"}},
		{Username: "club_core", Name: "Ядро", Ref: telegram.NewRef(telegram.KindChannel, 13)},
		{Username: "club_news", Name: "Анонсы", Ref: telegram.NewRef(telegram.KindChannel, 14), Broadcast: true, Unread: 3},
		{Ref: telegram.NewRef(telegram.KindChannel, 15)},
	}})

	result, _, err := handlers.dialogs(context.Background(), nil, dialogsArgs{})
	listed := toolText(t, result, err)
	for _, line := range []string{
		"человек: @ivan_petrov Иван, непрочитанных 2",
		"человек: Дмитрий стоматолог; публичного ника нет",
		"группа: Чат клуба, непрочитанных 7",
		"группа: @club_core Ядро",
		"канал: @club_news Анонсы, непрочитанных 3",
		"группа: название неизвестно",
	} {
		if !strings.Contains(listed, line) {
			t.Fatalf("в списке нет строки %q:\n%s", line, listed)
		}
	}
	if strings.Count(listed, noUsernameNote) != 1 {
		t.Fatalf("совет про tg_send стоит не только у человека:\n%s", listed)
	}
	if strings.Contains(listed, "[без текста]") {
		t.Fatalf("диалог без последнего сообщения напечатан пустой репликой:\n%s", listed)
	}
}

// Точное совпадение ника - тот, кого искали: в префиксной выдаче он стоит
// вперемешку с чужими похожими никами и теряется, поэтому идёт первым и с
// пометкой.
func TestExactUsernameGoesFirst(t *testing.T) {
	handlers := liveTools(&stubDialog{found: []Contact{
		{Username: "smirnovenko", Name: "Пётр"},
		{Username: "smirnov", Name: "Даниил", IsContact: true, Exact: true},
	}})

	result, _, err := handlers.find(context.Background(), nil, findArgs{Query: "smirnov"})
	text := toolText(t, result, err)
	if !strings.Contains(text, "@smirnov Даниил - точный ник - в контактах") {
		t.Fatalf("точный ник напечатан без пометки:\n%s", text)
	}
	if strings.Index(text, "@smirnov ") > strings.Index(text, "@smirnovenko") {
		t.Fatalf("точный ник не первый в списке:\n%s", text)
	}
}

// Похожие ники - другие люди: без предупреждения агент пишет первому в списке.
// Запрос нормализуется как в поиске, иначе «@SmirnovE» не признаётся ником и
// печатается с двойной собакой. По имени предупреждения нет: точного совпадения
// там не ждут.
func TestSimilarUsernamesAreWarnedAbout(t *testing.T) {
	ctx := context.Background()
	handlers := liveTools(&stubDialog{found: []Contact{{Username: "smirnovenko", Name: "Пётр"}}})

	// «@ SmirnovE» - тот же ник: пробел после собаки не делает запрос именем.
	for _, query := range []string{"@SmirnovE", "@ SmirnovE"} {
		result, _, err := handlers.find(ctx, nil, findArgs{Query: query})
		text := toolText(t, result, err)
		if !strings.Contains(text, "Среди найденных точного ника @smirnove нет") {
			t.Fatalf("похожие ники по запросу %q выданы без предупреждения:\n%s", query, text)
		}
	}

	byName, _, err := handlers.find(ctx, nil, findArgs{Query: "Пётр"})
	if shown := toolText(t, byName, err); strings.Contains(shown, "точного ника") {
		t.Fatalf("поиск по имени предупреждает о точном нике:\n%s", shown)
	}
}

// status по id отвечает про названную кампанию: итог прогона иначе недоступен -
// его перекрывает следующая кампания. Чужой id - отказ, а не отчёт без кампании:
// аккаунты вместо итога читаются как «кампания пуста».
func TestStatusAnswersByCampaignID(t *testing.T) {
	service := newService(t)
	handlers := &tools{service: service}
	ctx := context.Background()

	closed := startWith(t, service, person(1, "ivan_petrov"))
	if _, err := service.StopCampaign(ctx, true); err != nil {
		t.Fatalf("отменить кампанию: %v", err)
	}
	live, err := service.LoadCampaign(ctx, "Привет, {имя}!", "вторая")
	if err != nil {
		t.Fatalf("создать вторую кампанию: %v", err)
	}
	if _, err := service.AddRecipients(ctx, live, []Person{person(2, "petr_ivanov")}); err != nil {
		t.Fatalf("добавить получателей: %v", err)
	}
	if _, err := service.StartCampaign(ctx); err != nil {
		t.Fatalf("запустить вторую кампанию: %v", err)
	}

	current, _, err := handlers.status(ctx, nil, statusArgs{})
	if text := toolText(t, current, err); !strings.Contains(text, fmt.Sprintf("id %d", live)) {
		t.Fatalf("status без параметра показал не текущую кампанию:\n%s", text)
	}

	named, _, err := handlers.status(ctx, nil, statusArgs{CampaignID: closed})
	text := toolText(t, named, err)
	if !strings.Contains(text, fmt.Sprintf("id %d", closed)) || !strings.Contains(text, "cancelled") {
		t.Fatalf("status по id показал не названную кампанию:\n%s", text)
	}

	// Отрицательный id - та же опечатка, что и чужой: отчёт о текущей кампании в
	// ответ на него читается как отчёт о запрошенной.
	for _, id := range []int64{9999, -1} {
		_, _, err := handlers.status(ctx, nil, statusArgs{CampaignID: id})
		if err == nil {
			t.Fatalf("id %d принят как кампания", id)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%d", id)) {
			t.Fatalf("отказ не называет запрошенный id %d: %v", id, err)
		}
	}
}

// Аннотация ReadOnlyHint - обещание клиенту, что вызов ничего не меняет: с ней
// инструмент проходит без подтверждения. Обещать её у tg_send или start - значит
// пустить отправку наружу молча.
func TestReadToolsAreReadOnly(t *testing.T) {
	ctx := context.Background()
	serverSide, clientSide := mcp.NewInMemoryTransports()

	server := NewMCPServer(&Service{cfg: Config{AccountLabel: "test"}, Now: time.Now}, nil)
	serverSession, err := server.Connect(ctx, serverSide, nil)
	if err != nil {
		t.Fatalf("поднять сервер: %v", err)
	}
	defer func() { _ = serverSession.Close() }()

	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatalf("подключиться: %v", err)
	}
	defer func() { _ = session.Close() }()

	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("список инструментов: %v", err)
	}
	readOnly := map[string]bool{}
	for _, tool := range list.Tools {
		readOnly[tool.Name] = tool.Annotations != nil && tool.Annotations.ReadOnlyHint
	}
	for _, name := range []string{"tg_find", "tg_read", "tg_dialogs", "status"} {
		if !readOnly[name] {
			t.Fatalf("%s не помечен как читающий инструмент", name)
		}
	}
	for _, name := range []string{"tg_send", "start"} {
		if readOnly[name] {
			t.Fatalf("%s помечен как читающий, а он пишет наружу", name)
		}
	}
}

// start вне окна обязан назвать, когда отправка пойдёт: правило «окно 10:00-19:00»
// не отвечает на вопрос «а сейчас-то что», и запуск вечером пятницы выглядит как
// пошедшая отправка.
func TestStartOutsideWindowNamesOpening(t *testing.T) {
	service := newService(t)
	handlers := &tools{service: service}
	ctx := context.Background()

	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "тест")
	if err != nil {
		t.Fatalf("создать черновик: %v", err)
	}
	if _, err := service.AddRecipients(ctx, id, []Person{person(1, "ivan_petrov")}); err != nil {
		t.Fatalf("добавить получателей: %v", err)
	}
	// Суббота той же недели: окно закрыто до утра понедельника.
	service.Now = func() time.Time { return testNow.AddDate(0, 0, 3) }

	result, _, err := handlers.start(ctx, nil, struct{}{})
	text := toolText(t, result, err)
	if !strings.Contains(text, "окно отправки закрыто, ближайшее открытие 07.09 10:00 МСК") {
		t.Fatalf("start вне окна не назвал открытие:\n%s", text)
	}
}

// Совет в простое идёт по обстановке: черновик запускает start, а закрытая
// кампания заново не идёт - там start откажет, и создавать надо новую.
func TestIdleAdviceFollowsWhatCanStart(t *testing.T) {
	service := newService(t)
	handlers := &tools{service: service}
	ctx := context.Background()

	id, err := service.LoadCampaign(ctx, "Привет, {имя}!", "тест")
	if err != nil {
		t.Fatalf("создать черновик: %v", err)
	}
	if _, err := service.AddRecipients(ctx, id, []Person{person(1, "ivan_petrov")}); err != nil {
		t.Fatalf("добавить получателей: %v", err)
	}

	draft, _, err := handlers.status(ctx, nil, statusArgs{})
	if text := toolText(t, draft, err); !strings.Contains(text, "запустите её через start") {
		t.Fatalf("единственный черновик остался без совета start:\n%s", text)
	}

	if _, err := service.StopCampaign(ctx, true); err != nil {
		t.Fatalf("отменить черновик: %v", err)
	}
	closed, _, err := handlers.status(ctx, nil, statusArgs{})
	text := toolText(t, closed, err)
	if strings.Contains(text, "start") {
		t.Fatalf("по закрытой кампании советуется start:\n%s", text)
	}
	if !strings.Contains(text, "load_campaign") {
		t.Fatalf("по закрытой кампании не названо, чем создать новую:\n%s", text)
	}
}

// Сломанный шаблон называется у любой кампании, а снять её через stop
// советуется только живой: по закрытой stop откажет - снимать нечего.
func TestBrokenTemplateAdviceFitsTheCampaign(t *testing.T) {
	service := newService(t)
	handlers := &tools{service: service}
	ctx := context.Background()

	id := startWith(t, service, person(1, "ivan_petrov"))
	runSQL(t, service, `UPDATE campaigns SET template_text = 'Привет, {{имя}}!' WHERE id = $1`, id)

	live, _, err := handlers.status(ctx, nil, statusArgs{})
	if text := toolText(t, live, err); !strings.Contains(text, "негодную снимите через stop с cancel") {
		t.Fatalf("по идущей кампании нет совета снять её:\n%s", text)
	}

	if _, err := service.StopCampaign(ctx, true); err != nil {
		t.Fatalf("отменить кампанию: %v", err)
	}
	closed, _, err := handlers.status(ctx, nil, statusArgs{CampaignID: id})
	text := toolText(t, closed, err)
	if !strings.Contains(text, "Шаблон этой кампании не разобран") {
		t.Fatalf("по закрытой кампании не назван сломанный шаблон:\n%s", text)
	}
	if strings.Contains(text, "stop с cancel") {
		t.Fatalf("по закрытой кампании советуется stop:\n%s", text)
	}
}

// Сообщение без текста и вложения: пустая строка «он: » не сообщает читателю
// ничего. Нулевое время у такой строки тоже не должно её ломать.
func TestEmptyMessageIsNamed(t *testing.T) {
	handlers := liveTools(&stubDialog{
		poll: func(int) []ChatMessage { return []ChatMessage{{ID: 1}} },
	})

	result, _, err := handlers.read(context.Background(), nil, readArgs{Username: "ivan_petrov"})
	text := toolText(t, result, err)
	if !strings.Contains(text, "  он: [без текста]\n") {
		t.Fatalf("сообщение без текста напечатано пустым:\n%s", text)
	}
}

// agentService - сервис с обоими токенами и списком агента; базы не нужно.
func agentService() *Service {
	return &Service{cfg: Config{
		AccountLabel:   "test",
		MCPToken:       "owner-token",
		MCPAgentToken:  "agent-token",
		AgentAllowList: []string{"dev_account"},
	}, Now: time.Now}
}

// bearer подставляет токен в каждый запрос клиента MCP.
type bearer struct{ token string }

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(req)
}

// toolsBehind подключается к эндпоинту с токеном и отдаёт имена инструментов.
func toolsBehind(t *testing.T, endpoint, token string) (map[string]bool, error) {
	t.Helper()
	ctx := context.Background()
	transport := &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           &http.Client{Transport: bearer{token}},
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()
	list, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, tool := range list.Tools {
		names[tool.Name] = true
	}
	return names, nil
}

// Токен определяет набор инструментов: агенту видны только два диалоговых, и
// кампании, стоп аккаунта и чужая переписка ему недоступны вовсе. Точный набор,
// а не отсутствие одного start: утёкший tg_dialogs иначе прошёл бы тест.
func TestTokenPicksTools(t *testing.T) {
	service := agentService()
	server := httptest.NewServer(MCPHandler(service, offlineChat(&stubDialog{})))
	t.Cleanup(server.Close)

	owner, err := toolsBehind(t, server.URL, "owner-token")
	if err != nil {
		t.Fatalf("владелец не подключился: %v", err)
	}
	if len(owner) != 10 {
		t.Fatalf("владельцу видно %d инструментов, ждали 10: %v", len(owner), owner)
	}

	agent, err := toolsBehind(t, server.URL, "agent-token")
	if err != nil {
		t.Fatalf("агент не подключился: %v", err)
	}
	if len(agent) != 2 || !agent["tg_send"] || !agent["tg_read"] {
		t.Fatalf("агенту видно %v, ждали ровно tg_send и tg_read", agent)
	}

	if _, err := toolsBehind(t, server.URL, "wrong-token"); err == nil {
		t.Fatal("чужой токен пущен")
	}
}

// Сессия принадлежит токену, который её открыл: агентский токен не
// продолжает сессию владельца, и наоборот. Иначе агент получил бы кампании,
// узнав номер сессии.
func TestSessionBelongsToItsToken(t *testing.T) {
	server := httptest.NewServer(MCPHandler(agentService(), offlineChat(&stubDialog{})))
	t.Cleanup(server.Close)

	post := func(token, session, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp
	}
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	list := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`

	for _, pair := range [][2]string{{"owner-token", "agent-token"}, {"agent-token", "owner-token"}} {
		opened := post(pair[0], "", initialize)
		session := opened.Header.Get("Mcp-Session-Id")
		if session == "" {
			t.Fatalf("%s: сессия не открылась, код %d", pair[0], opened.StatusCode)
		}
		if resp := post(pair[1], session, list); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("сессия %s продолжена токеном %s: код %d", pair[0], pair[1], resp.StatusCode)
		}
	}
}

// Агент пишет и читает только тестовые аккаунты, и отказывает сам сервис до
// похода в Telegram. Вызовы идут через собранный NewAgentServer: подмена
// обработчика или списка в сборке роняет тест. Ник приводится той же
// нормализацией, что в Chat: запись с @, в другом регистре или ссылкой не
// обходит список и не ломает законный ник.
func TestAgentReachesOnlyTestAccounts(t *testing.T) {
	ctx := context.Background()
	notReady := &telegram.Failure{Kind: telegram.KindNotReady, Message: "клиент не запущен"}
	service := agentService()
	service.cfg.OwnerAccounts = []string{"smirnov"}
	server := NewAgentServer(service, offlineChat(&failDialog{err: notReady}))

	serverSide, clientSide := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverSide, nil)
	if err != nil {
		t.Fatalf("поднять сервер: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatalf("подключиться: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	cases := []struct {
		username string
		allowed  bool
	}{
		{"dev_account", true},
		{"@Dev_Account", true},
		{"t.me/dev_account", true},
		{"smirnov", false},
		{"", false},
		{"dev_account/x", false},
	}
	for _, tc := range cases {
		// Пустой текст отбивает Chat.Send до базы и Telegram: отказ про текст
		// и значит, что вызов прошёл список агента.
		calls := map[string]map[string]any{
			"tg_send": {"username": tc.username, "text": ""},
			"tg_read": {"username": tc.username},
		}
		for tool, arguments := range calls {
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: arguments})
			if err != nil {
				t.Fatalf("%s %q: вызов: %v", tool, tc.username, err)
			}
			var text strings.Builder
			for _, content := range result.Content {
				if item, ok := content.(*mcp.TextContent); ok {
					text.WriteString(item.Text)
				}
			}
			refused := strings.Contains(text.String(), "AGENT_ALLOW_LIST")
			if refused == tc.allowed {
				t.Fatalf("%s %q: ждали пропуск %v, ответ %q", tool, tc.username, tc.allowed, text.String())
			}
		}
	}
}
