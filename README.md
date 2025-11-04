# telegram-userbot

MTProto‑юзербот на Go 1.25, построенный на базе [gotd](https://github.com/gotd/td).  
Следит за входящими сообщениями, прогоняет каждое через набор **фильтров** (`filters.json`), и отправляет **уведомление** либо через пользовательский аккаунт (MTProto‑клиент), либо через Bot API. Поддерживаются троттлинг, идемпотентность, график рассылок, сглаживание всплесков правок, кэш пиров и корректный graceful‑shutdown.

> Кратко: заполнили `.env`, описали правила в `filters.json`, запустили `cmd/userbot` и получили управляемый юзербот с интерактивной CLI и устойчивой очередью уведомлений.

---

## Возможности

- **MTProto‑клиент** с интерактивной авторизацией (номер, код, 2FA), сохранением сессии и устойчивым переподключением.
- **Фильтры**: `keywords_any`, `keywords_all`, `regex`, отрицания `exclude_any` и `exclude_regex`; источники по списку чатов/пользователей/каналов.
- **Очередь уведомлений**:
  - два контура доставки: `urgent` (уведомления отправляются немедленно) и `regular` (добавляются в очередь и уходят по расписанию), FIFO;
  - персист на диск с атомарной записью, журнал неудачных уведомлений;
  - идемпотентность через детерминированные `random_id`.
- **Доставка**: через MTProto‑клиента или через **Bot API**; у Bot API учитывается `retry_after`, у MTProto — `FLOOD_WAIT` с джиттером.
- **Lifecycle:** каскадная регистрация узлов и корректный graceful shutdown.
- **Общий троттлер.** Token bucket, экспоненциальный backoff.
- **Стабилизация входящих**: дедупликация апдейтов, дебаунс частых правок одного сообщения.
- **Кэш пиров Telegram**: users/chats/channels и `InputPeer*`, плюс извлечение по `entities`.
- **Интерактивная CLI**: `help`, `list`, `reload`, `status`, `flush`, `whoami`, `version`, `exit`.
- **MarkRead**: периодическая отметка фильтруемых чатов прочитанными.
- **Статус**: При доставке через MTProto‑клиента управление статусом `online/typing`, авто‑offline с задержкой.

---

## Быстрый старт

### 1) Зависимости

- Go **1.25**+
- Данные Telegram API: `API_ID`, `API_HASH`
- Необязательно: `BOT_TOKEN` если хотите слать через Bot API

### 2) Клонирование и сборка

```bash
git clone <repo-url> telegram-userbot
cd telegram-userbot
go mod download
```

### 3) Настроить окружение (`.env`)

Скопируйте пример и заполните:

```bash
cp assets/.env.example assets/.env
```

Ключевые переменные:

| Переменная | Значение | По умолчанию |
|---|---|---|
| `API_ID`, `API_HASH`, `PHONE_NUMBER` | учетные данные MTProto | — (обязательно) |
| `SESSION_FILE` | путь к файлу сессии | `data/session.bin` |
| `STATE_FILE` | файл состояния апдейтов gotd | `data/state.json` |
| `NOTIFIER` | `client` или `bot` | `client` |
| `BOT_TOKEN` | токен бота, если `NOTIFIER=bot` | — |
| `THROTTLE_RPS` | целевые запросы/сек | `1` |
| `DEDUP_WINDOW_SEC` | окно дедупликации апдейтов | `120` |
| `DEBOUNCE_EDIT_MS` | ожидание «последней правки» | `2000` |
| `NOTIFY_QUEUE_FILE` | файл очереди | `data/notify_queue.json` |
| `NOTIFY_FAILED_FILE` | файл провалов | `data/notify_failed.json` |
| `NOTIFIED_CACHE_FILE` | кэш «что уже уведомляли» | `data/notified_cache.json` |
| `NOTIFIED_CACHE_TTL_DAYS` | TTL кэша уведомлений | `30` |
| `NOTIFY_TIMEZONE` | часовой пояс расписания | `Europe/Moscow` |
| `NOTIFY_SCHEDULE` | расписание уведомлений, формат `HH:MM[,HH:MM...]` | `08:00,17:00` |
| `RECIPIENTS_FILE` | файл с определениями получателей | `assets/recipients.json` |
| `LOG_LEVEL` | `debug`/`info`/`warn`/`error` | `debug` |
| `TEST_DC` | `true` для тестового DC (MTProto и Bot API) | `false` |
| `ADMIN_UID` | UID администратора для сервисных уведомлений и для команды `test` | `0` |

> Примечание: минимальный .env должен включать `API_ID`, `API_HASH`, `PHONE_NUMBER`, остальные значения будут взяты по умолчанию, но при загрузке в лог попадут предупреждения.

Приоритет настроек: **CLI‑флаги > переменные окружения > значения по умолчанию**.

### 4) Описать получателей (`recipients.json`)

Создайте файл с определением получателей:

```bash
cp assets/recipients.json.example assets/recipients.json
```

**Расположение:** `assets/recipients.json` (настраивается через переменную окружения `RECIPIENTS_FILE`)

**Формат:**
- `recipients` - мапа определений получателей, ключ - уникальный ID получателя
  - `kind` - тип получателя: `user`, `chat`, или `channel` (по умолчанию: `user`)
  - `peer_id` - **обязательное поле**, Telegram peer ID
  - `note` - опциональное описание или отображаемое имя
  - `tz` - опциональная временная зона (IANA имя или UTC смещение в формате "+03:00")
  - `schedule` - опциональное расписание доставки в формате массива "HH:MM"

**Пример:**
```json
{
  "recipients": {
    "admin_main": {
      "kind": "user",
      "peer_id": 5002402758,
      "note": "Main administrator",
      "tz": "Europe/Moscow",
      "schedule": ["09:00", "18:00"]
    },
    "chat_team": {
      "kind": "chat",
      "peer_id": 45678902232,
      "note": "Team chat"
    }
  }
}
```

### 5) Описать правила (`filters.json`)

Скопируйте пример:

```bash
cp assets/filters.json.example assets/filters.json
```

Структура:

```json
{
  "filters": [
    {
      "id": "support-alerts",
      "chats": [123456789, 987654321],
      "match": {
        "keywords_any": ["urgent", "incident", "pager"],
        "keywords_all": ["site", "down"],
        "regex": "(?i)sev[12]",
        "exclude_any": ["test", "sandbox"],
        "exclude_regex": "\\bdebug\\b"
      },
      "urgent": true,
      "notify": {
        "recipients": ["admin_main", "chat_team"],
        "forward": false,
        "template": "Искомые слова: {{keywords}}\\nregex: {{regex}}\\nСсылка: {{message_link}}"
      }
    }
  ]
}
```

**Формат:** чистый JSON без комментариев. Пояснения к полям:

- `chats` — ID диалогов; получить можно командой `list` в CLI.
- `match` — участвуют только присутствующие подполя; каждое присутствующее условие обязано совпасть:
  - `keywords_any` — должно совпасть хотя бы одно слово;
  - `keywords_all` — должны совпасть все слова;
  - `regex` — регулярное выражение по тексту сообщения; для регистронезависимости используйте префикс `(?i)`;
  - исключения `exclude_any` и `exclude_regex` имеют приоритет: одно срабатывание отклоняет сообщение.
- `urgent` — при значении `true` уведомление минует расписание и отправляется сразу; иначе попадает в очередь и уйдет в ближайшее окно из `NOTIFY_SCHEDULE`.
- `notify.recipients` — массив строк ID получателей из `recipients.json`. Все указанные ID должны существовать в `recipients.json`.
- `template` — строка с плейсхолдерами (см. ниже).

**Изменения по сравнению с предыдущей версией:**
- `notify.recipients` теперь массив строк ID получателей (вместо inline peer списков)
- Получатели должны быть определены в `recipients.json` сначала

Подстановки в шаблоне: `{{keywords}}`, `{{regex}}`, `{{message_link}}`. Ссылки строятся как `https://t.me/<username>/<message_id>` при наличии username; иначе формируется `tg://` ссылка.

### 5) Запуск

Во время первого запуска потребуется авторизация (код, возможный пароль 2FA). Сессия сохранится в `SESSION_FILE`.

```bash
# вариант 1: бинарник
go build -o ./bin/telegram-userbot ./cmd/userbot
./bin/telegram-userbot -env assets/.env -filters assets/filters.json

# вариант 2: из исходников
go run ./cmd/userbot -env assets/.env -filters assets/filters.json
```

Остановка: `Ctrl+C`. Приложение делает graceful‑shutdown, дожидаясь дренирования очередей и смены статуса на `offline`.

---

## Как это работает

### Поток событий

1. **MTProto‑клиент** получает апдейты, менеджер соединений помечает `online`.
2. **Handlers** из `internal/domain/updates` стабилизируют входящие:
   - дедуп по `(peerID,msgID,editDate)`,
   - дебаунс частых правок одного сообщения,
   - кэш «уже уведомляли» с TTL.
3. **Фильтры** из `internal/domain/filters` проверяют `keywords/regex/exclude` и источники.
4. **Очередь** (`internal/domain/notifications`) ставит `Job` в `urgent` или `regular`.
5. **Доставка**:
   - `client`‑режим: через MTProto, учитывая `FLOOD_WAIT` и с `random_id` для идемпотентности;
   - `bot`‑режим: через Bot API, уважая `retry_after`.
6. **MarkRead** планово отмечает чаты прочитанными.
7. **Статус/тайпинг** аккуратно отражают активность и уход в `offline` при простое.

### Устойчивость

- Все критичные структуры пишутся **атомарно** (`internal/infra/storage`).
- Очередь и кэши восстанавливаются при рестарте.
- Троттлинг: токен‑бакет + backoff с джиттером; внешние «подожди» обрабатываются экстракторами.

---

## Директории

```
cmd/userbot/                     # точка входа CLI
internal/
  adapters/
    telegram/core/               # клиент gotd: auth, client, state storage
    telegram/notifier/           # доставка через MTProto
    botapi/notifier/             # доставка через Bot API
    cli/                         # консоль
  domain/
    filters/                     # движок сопоставления правил
    updates/                     # обработка апдейтов, notified‑кэш, mark‑read
    notifications/               # модели и очередь уведомлений
  infra/
    telegram/{connection,status,runtime,cache}  # соединение, статус, утилиты
    throttle/                    # троттлер и backoff
    lifecycle/                   # менеджер запуска/остановки сервисов
    storage/                     # EnsureDir, AtomicWriteFile
    logger/, pr/                 # логгер и интеграция с readline
  support/
    version/                     # Name/Version
assets/                          # .env.example, filters.json.example
data/                            # файлы по умолчанию (в .gitignore)
```

---

## CLI команды

В интерактивной консоли доступны:

- `help` — список команд  
- `list` — распечатать кэшированные диалоги  
- `reload` — перечитать `filters.json`  
- `status` — размеры очереди, последний дрен, следующий слот расписания  
- `flush` — немедленно дренировать regular‑очередь  
- `test` — отправить сообщение администратору (проверка связности)  
- `whoami` — информация об аккаунте  
- `version` — версия приложения  
- `exit` — остановить CLI и завершить сервис

---

## Полезные советы

- **Первый запуск**: держите рядом устройство с номером и кодом, а также пароль 2FA, если включен.
- **Bot API**: задайте `NOTIFIER=bot` и `BOT_TOKEN=...`. В этом режиме форвард работает как пересылка от бота, не от пользователя.
- **Расписание**: `NOTIFY_SCHEDULE` — CSV, формат `HH:MM` в `NOTIFY_TIMEZONE`. `urgent=true` минует расписание.
- **Логи**: `LOG_LEVEL=debug` поможет на старте. В проде уменьшите шум.
- **FLOOD_WAIT/retry_after**: троттлер сам подождёт нужное время. Не пытайтесь «ускорить» это настройками RPS.

#### Git‑гигиена

Рекомендуемый `.gitignore`:
```gitignore
/bin/
/data/
assets/.env
```

---

## Зависимости

- [gotd/td](https://github.com/gotd/td) — Telegram MTProto SDK для Go
- [zap](https://github.com/uber-go/zap) — логирование
- [readline](https://github.com/chzyer/readline) — CLI
- и другие зависимости в `go.mod`

---

## Требования
- Go **1.25+**
- Аккаунт Telegram для MTProto, Bot API токен (для доставки через бота).

---

## Предостережения

Telegram формально допускает MTProto‑клиентов, но на практике аккаунты могут получать ограничения или бан за автоматизацию, массовые рассылки и иные нарушения правил. Используйте на свой риск.

Рекомендации:
- держите `THROTTLE_RPS` низким, уважайте `FLOOD_WAIT`/`retry_after` — приложение само подождет нужное время;
- не отправляйте массовые сообщения незнакомым пользователям и в публичные чаты без согласия;
- по возможности используйте отдельный рабочий аккаунт;
- ожидайте проверки/заморозки/блокировки без предупреждений;
- храните сессию и токены только локально, не коммитьте `assets/.env`, `data/*` и файлы очередей.

---

## Лицензия

MIT
