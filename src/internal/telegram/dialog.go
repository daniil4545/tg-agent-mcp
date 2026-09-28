package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/gotd/contrib/storage"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/tg"
)

// Found - человек, найденный поиском.
type Found struct {
	Ref      Ref
	Username string
	Name     string
	// IsContact - человек в адресной книге аккаунта.
	IsContact bool
	// Known - адрес человека уже лежал в локальном хранилище: с ним
	// переписывались, его ник резолвили или он попал в прогрев. Наличия диалога
	// это не означает - резолв ника под чтение кладёт туда же.
	Known bool
	// Exact - ник человека равен запросу без учёта регистра. Префиксный поиск
	// отдаёт похожие ники чужих людей, и без пометки они неотличимы от того,
	// кого искали.
	Exact bool
}

// usernameForm - что Telegram вообще принимает за ник. Без проверки поиск по
// имени тратил бы резолв на заведомо не-ник, а очередь - минуту на строку.
var usernameForm = regexp.MustCompile(`^[a-z][a-z0-9_]{3,30}[a-z0-9]$`)

// IsUsername - строка по форме ник Telegram: 5-32 символа латиницей, цифры и
// подчёркивание, не в конце и не двойное. Ведущая @ и регистр снимаются
// CleanQuery, второй нормализации здесь нет.
func IsUsername(name string) bool {
	return usernameForm.MatchString(name) && !strings.Contains(name, "__")
}

// CleanQuery приводит запрос поиска к виду, в котором он сравнивается с ником:
// без пробелов и ведущей @, в нижнем регистре. Ответ агенту показывает тот же
// вид, что искали, иначе «@Ivan» дал бы в тексте «@@».
// Пробелы снимаются дважды: «@ smirnove» после снятия @ остаётся с ведущим
// пробелом и перестаёт быть ником по форме.
func CleanQuery(query string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(query), "@")))
}

// Dialog - строка списка диалогов: адресат и последнее сообщение.
type Dialog struct {
	Peer     Ref
	Username string
	Name     string
	Unread   int
	// Broadcast - флаг канала на стороне Telegram: вещание, а не переписка.
	// Голый факт платформы без толкования: слово для выдачи собирает вызывающий
	// из вида ссылки и этого флага.
	Broadcast bool
	Last      Message
}

// SearchUsers ищет человека по нику или имени.
//
// Сначала локальное хранилище адресатов - ноль запросов к Telegram и полный
// ответ по тем, кто уже известен; затем точный резолв ника, если локально его
// не нашлось; затем поиск на сервере.
func (c *Client) SearchUsers(ctx context.Context, query string, limit int) ([]Found, error) {
	api, peers, err := c.session()
	if err != nil {
		return nil, err
	}
	needle := CleanQuery(query)
	if needle == "" {
		return nil, permanent(KindBadRequest, "пустой запрос поиска")
	}
	if limit <= 0 {
		return nil, permanent(KindBadRequest, "лимит поиска должен быть положительным")
	}

	found := make([]Found, 0, limit)
	seen := make(map[int64]bool, limit)
	var exact bool

	iter, err := peers.Iterate(ctx)
	if err != nil {
		return nil, fmt.Errorf("iterate telegram peers: %w", err)
	}
	for len(found) < limit && iter.Next(ctx) {
		user := iter.Value().User
		if user == nil || seen[user.GetID()] || !matches(user, needle) {
			continue
		}
		seen[user.GetID()] = true
		hit := strings.ToLower(publicUsername(user)) == needle
		exact = exact || hit
		found = append(found, Found{
			Ref:       NewRef(KindUser, user.GetID()),
			Username:  publicUsername(user),
			Name:      fullName(user),
			IsContact: user.GetContact(),
			Known:     true,
			Exact:     hit,
		})
	}
	iterErr := iter.Err()
	if closeErr := iter.Close(); closeErr != nil {
		slog.Warn("close telegram peer iterator", "error", closeErr)
	}
	if iterErr != nil {
		return nil, fmt.Errorf("iterate telegram peers: %w", iterErr)
	}

	// doubt - резолв точного ника сорвался, и «человека нет» из этого не следует:
	// contacts.resolveUsername жёстко лимитирован на живом номере и отвечает
	// FLOOD_WAIT, пока contacts.search работает и возвращает пусто.
	var doubt error
	if !exact && IsUsername(needle) {
		hit, id, err := c.resolveExact(ctx, peers, needle)
		switch {
		case err != nil:
			// «Ник не занят» - штатный исход поиска по имени, похожему на ник, а не
			// сбой: WARN на нём становится фоном ровно в том логе, где положено
			// замечать стопы аккаунта. В лог идёт только неполученный ответ.
			if unchecked(err) {
				slog.Warn("exact username resolve failed", "username", needle, "error", err)
				doubt = err
			}
		case !seen[id]:
			seen[id] = true
			found = append([]Found{hit}, found...)
		default:
			// Тот же человек уже найден локально по устаревшему нику из peer cache:
			// ник он сменил, id остался. Отбросить резолв целиком - сказать «точного
			// ника нет» о том, кого искали.
			ref := NewRef(KindUser, id)
			for i := range found {
				if found[i].Ref == ref {
					found[i].Exact = true
					found[i].Username = hit.Username
					break
				}
			}
		}
	}
	if len(found) >= limit {
		return found[:limit], nil
	}

	result, err := api.ContactsSearch(ctx, &tg.ContactsSearchRequest{Q: needle, Limit: limit})
	if err != nil {
		return nil, classify(err)
	}
	users := tg.UserClassArray(result.Users).UserToMap()
	// MyResults - совпадения из адресной книги: сервер отделяет их от общего
	// поиска, и это единственный признак контакта для незнакомого нам ответа.
	mine := make(map[int64]bool, len(result.MyResults))
	for _, item := range result.MyResults {
		if peer, ok := item.(*tg.PeerUser); ok {
			mine[peer.UserID] = true
		}
	}
	for _, batch := range [][]tg.PeerClass{result.MyResults, result.Results} {
		for _, item := range batch {
			if len(found) >= limit {
				break
			}
			peer, ok := item.(*tg.PeerUser)
			if !ok || seen[peer.UserID] {
				continue
			}
			user, ok := users[peer.UserID]
			if !ok {
				continue
			}
			seen[peer.UserID] = true
			found = append(found, Found{
				Ref:       NewRef(KindUser, peer.UserID),
				Username:  publicUsername(user),
				Name:      fullName(user),
				IsContact: user.GetContact() || mine[peer.UserID],
				// Префиксный поиск возвращает и точный ник, если резолв до него не
				// дошёл или сорвался.
				Exact: strings.ToLower(publicUsername(user)) == needle,
			})
		}
	}
	// Проверить точный ник не удалось, а нечёткий поиск ничего не дал: «никого не
	// нашлось» здесь враньё - человек может существовать, и tg_send напишет ему в
	// ту же секунду. Наружу идёт причина срыва.
	if len(found) == 0 && doubt != nil {
		return nil, doubt
	}
	return found, nil
}

// unchecked - резолв ника не дал ответа о человеке: лимит, обрыв или мёртвая
// сессия. Отказ по существу («ник не занят», «ник не человек») - это ответ, и
// пустая выдача после него правдива; лимит, обрыв и 5xx не значат ничего, и
// выдавать их за «никого не нашлось» нельзя.
func unchecked(err error) bool {
	var failure *Failure
	if !errors.As(err, &failure) {
		return true
	}
	switch failure.Kind {
	case KindFloodWait, KindPeerFlood, KindNotReady, KindAuthForbidden:
		return true
	}
	return !failure.NotSent()
}

// resolveExact - точное совпадение по нику через ResolveUsername. Отдельный
// запрос нужен потому, что незнакомца нет ни в локальном хранилище, ни в
// выдаче contacts.search: без резолва tg_find отвечает «никого» о человеке,
// которому tg_send написал бы.
func (c *Client) resolveExact(ctx context.Context, peers storage.PeerStorage, needle string) (Found, int64, error) {
	ref, err := c.ResolveUsername(ctx, needle)
	if err != nil {
		return Found{}, 0, err
	}
	id, err := ref.UserID()
	if err != nil {
		return Found{}, 0, err
	}
	// Имя берём из хранилища: ResolveUsername только что положил туда человека,
	// и второго запроса ради имени не нужно.
	peer, err := storage.FindPeer(ctx, peers, &tg.PeerUser{UserID: id})
	if err != nil {
		return Found{}, 0, fmt.Errorf("find resolved peer @%s: %w", needle, err)
	}
	if peer.User == nil {
		return Found{}, 0, permanent(KindNotPerson, "@"+needle+" не человек")
	}
	return Found{
		Ref:       ref,
		Username:  publicUsername(peer.User),
		Name:      fullName(peer.User),
		IsContact: peer.User.GetContact(),
		Exact:     true,
	}, id, nil
}

// History отдаёт страницу переписки от старых сообщений к новым. beforeID
// ограничивает выдачу сообщениями старше него, ноль означает «с последнего».
func (c *Client) History(ctx context.Context, peer Ref, limit, beforeID int) ([]Message, error) {
	api, peers, err := c.session()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, permanent(KindBadRequest, "глубина истории должна быть положительной")
	}
	target, err := resolve(ctx, peers, peer)
	if err != nil {
		return nil, err
	}

	response, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:     target,
		Limit:    limit,
		OffsetID: beforeID,
	})
	if err != nil {
		return nil, classify(err)
	}
	modified, ok := response.AsModified()
	if !ok {
		return nil, permanent(KindBadRequest, "Telegram не вернул историю")
	}

	entities := tg.Entities{Users: tg.UserClassArray(modified.GetUsers()).UserToMap()}
	items := modified.GetMessages()
	// Telegram отдаёт историю новыми вперёд, а читают её сверху вниз, поэтому
	// обход идёт с конца. Служебные и пустые сообщения пропускаются.
	history := make([]Message, 0, len(items))
	for i := len(items) - 1; i >= 0; i-- {
		item, ok := items[i].(*tg.Message)
		if !ok {
			continue
		}
		message, ok := fromMessage(item, entities)
		if !ok {
			continue
		}
		history = append(history, message)
	}
	return history, nil
}

// Dialogs отдаёт диалоги всех трёх видов - человек, группа, канал, - новые
// сверху.
func (c *Client) Dialogs(ctx context.Context, limit int) ([]Dialog, error) {
	api, peers, err := c.session()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, permanent(KindBadRequest, "лимит диалогов должен быть положительным")
	}

	// Счётчик ручной: Collect у библиотеки обходит все диалоги аккаунта до
	// конца, а их на живом номере сотни.
	iter := query.GetDialogs(api).BatchSize(dialogsBatch).Iter()
	dialogs := make([]Dialog, 0, limit)
	for len(dialogs) < limit && iter.Next(ctx) {
		item := iter.Value()
		// Приведение обязательно: у DialogClass три вида, и адресат есть не у
		// каждого.
		dialog, ok := item.Dialog.(*tg.Dialog)
		if !ok {
			continue
		}
		ref, ok := refFromPeer(dialog.Peer)
		if !ok {
			continue
		}

		entities := tg.Entities{
			Users:    item.Entities.Users(),
			Chats:    item.Entities.Chats(),
			Channels: item.Entities.Channels(),
		}
		found := Dialog{Peer: ref, Unread: dialog.UnreadCount}
		switch peer := dialog.Peer.(type) {
		case *tg.PeerUser:
			found.Username = username(entities, peer.UserID)
			found.Name = name(entities, peer.UserID)
		case *tg.PeerChat:
			// У обычной группы нет ни ника, ни access hash: она адресуется одним id.
			if chat, ok := entities.Chats[peer.ChatID]; ok {
				found.Name = chat.Title
			}
		case *tg.PeerChannel:
			channel, ok := entities.Channels[peer.ChannelID]
			if !ok {
				break
			}
			found.Name, found.Broadcast = channel.Title, channel.Broadcast
			found.Username = pickUsername(channel.Username, channel.Usernames)
			// Единственное место, где берётся access hash канала: у приватной
			// супергруппы ника нет, и резолвом его потом не достать. FromChat
			// отвечает false на min-конструкторе - такой хеш непригоден.
			var stored storage.Peer
			if stored.FromChat(channel) {
				// Кэш хеша - побочная выгода списка, а не то, ради чего звали
				// инструмент: отказ записи не отменяет уже разобранную выдачу.
				if err := peers.Add(ctx, stored); err != nil {
					slog.Warn("persist dialog channel", "channel_id", peer.ChannelID, "error", err)
				}
			}
		}
		if last, ok := item.Last.(*tg.Message); ok {
			if message, ok := fromMessage(last, entities); ok {
				found.Last = message
			}
		}
		dialogs = append(dialogs, found)
	}
	if err := iter.Err(); err != nil {
		return nil, classify(err)
	}
	return dialogs, nil
}

// pickUsername - публичный ник по паре полей Telegram: основное поле, затем
// первый активный из коллекционных. Поля устроены одинаково у человека и у
// канала, поэтому разбор один на оба вида.
func pickUsername(main string, extra []tg.Username) string {
	if main != "" {
		return main
	}
	for _, candidate := range extra {
		if candidate.Active && candidate.Username != "" {
			return candidate.Username
		}
	}
	return ""
}

// matches - грубое совпадение по нику и имени без учёта регистра. Точный ник
// ищет ResolveUsername, а здесь человека вспоминают по обрывку.
func matches(user *tg.User, needle string) bool {
	return strings.Contains(strings.ToLower(publicUsername(user)), needle) ||
		strings.Contains(strings.ToLower(fullName(user)), needle)
}
