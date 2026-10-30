# План: апстрим-триаж (v0.84.0 → v0.87.0) + question tool — 2026-07-19

Триаж 176 коммитов `charmbracelet/crush` (v0.84.0 `355fb647` → v0.87.0
`677046d2`), продолжение батча от `docs/plans/2026-07-10-upstream-port-plan.md`
(тот покрывал v0.72.0 → v0.84.0). Плюс — новая фича `question tool`,
спроектированная в этой же сессии на основе паттерна `PeakHoursError`/
`PeakHoursGuidance` (`internal/agent/peak_hours_stop.go`).

Правило то же: cherry-pick/порт по одному, `git apply --check` (или прямой
`cherry-pick`) до правки, после — `go build ./...` + тесты затронутых
пакетов, отдельный коммит.

## Правка CLAUDE.md (сделана в этой сессии, требует отдельного коммита)

Перед стартом порта — зафиксировать в `CLAUDE.md` два вывода этой сессии:

1. **Мультисессионный движок форка старше и зрелее апстримного** —
   lock-file + heartbeat модель не имеет отношения к их проблеме "один
   workspace на несколько живых UI-клиентов". Гонки в общем коде
   (`coordinator.go`, `agent.go`) — **EVAL, не PORT**: сначала проверить,
   воспроизводима ли гонка через НАШУ сессионную модель.
2. **LSP полностью удалён из форка** — `internal/lsp/` не существует,
   нигде не импортируется. Весь апстримный LSP-кластер (включая новые
   `lsp_definition`/`lsp_rename`/`lsp_call_hierarchy`/`lsp_symbols` tools)
   → SKIP целиком, даже если едет в одном коммите с чем-то ещё.

(Оба пункта уже внесены в рабочую копию `CLAUDE.md` в этой сессии —
осталось закоммитить.)

## Фаза 1 — низкорисковые PORT-кандидаты

Проверены через `git apply --check` против `origin/main` — применяются
чисто, независимые файлы, можно делать параллельно/по одному не глядя
друг на друга:

| # | Коммит | Файл(ы) | Apply-check | Заметка |
|---|---|---|---|---|
| 1.1 | `213ad794` | `internal/config/config_windows.go` (новый) | ✅ чисто | no-op `systemConfigPath` на Windows — тривиально |
| 1.2 | из батча OAuth-фиксов | `internal/oauth/token.go` | ✅ чисто | `TokenExchangeError` + персистентный `OAuthClient` для рефреша без повторного discovery — у нас этих типов пока нет, добавляются, не заменяют |
| 1.3 | часть `73031584`-соседей | `internal/shell/coreutils_exec.go` + `coreutils_exec_stub.go` (новые) | ✅ чисто | Go-native coreutils fallback (`mvdan.cc/sh/moreinterp/coreutils`) для shell exec — полезно для restricted/headless окружений |
| 1.4 | `132a8c89` | `internal/agent/tools/mcp/process_unix.go` + `process_other.go` (новые) | ✅ чисто | reap orphaned MCP stdio process groups — тот же класс бага, что мы чинили в `exec_windows.go` этой сессией. См. Фазу 2 — проверить, нужен ли Windows-эквивалент отдельно |
| 1.5 | `677046d2` | `internal/ui/notification/native.go` | ❌ конфликт | illumos build fix — низкий приоритет, руками адаптировать при желании, не блокер |

## Фаза 2 — EVAL, требует ручной сверки против кода форка

Не блочный cherry-pick, по одному, с чтением диффа:

1. **`internal/agent/tools/mcp/process_unix.go`/`process_other.go`
   (после порта из 1.4)** — проверить, есть ли у MCP-процессов на Windows
   та же дыра, что была у background-shell до `66bfc158`. Если да —
   написать Windows-эквивалент по образцу `exec_windows_test.go`'шного
   self-re-exec теста.
2. **Coordinator dispatch race** (`ae9257b9`/`fbf59341`/`492460a8`,
   рефактор координатора в struct + fix "serialize in-process run
   dispatch to prevent concurrent turns") — **по новому правилу CLAUDE.md**:
   сначала подтвердить, что гонка вообще достижима через lock-file+heartbeat
   модель (наш `coordinator.go` изначально "drives N concurrent sessions",
   возможно другой источник истины). Не портировать вслепую только потому
   что апстрим добавил тест.
3. **Edit tool refactor** (`internal/agent/tools/edit.go`, `multiedit.go`,
   `sourcegraph.go` — дедуп find/replace, лимиты Sourcegraph) — проверить
   конфликты с нашими правками этих же файлов.
4. **MCP OAuth** (`internal/oauth/mcp/*`, ~1000 новых строк, самодостаточный
   пакет) — ОТДЕЛЬНЫЙ вопрос пользователю, нужен ли вообще сейчас. Не
   включать в основную последовательность без явного да.

## Фаза 3 — Question tool

Спроектирован в этой сессии. **Важная поправка после проверки кода**:
изначальный дизайн предполагал отдельную "interactive" ветку с блокирующим
каналом по образцу `permission.Request` — это было ошибкой. Проверено:
`internal/server/handlers.go:163-171` — веб-сессии тоже безусловно
auto-approve, никакого блокирующего permission-диалога к UI не подключено
(`ConfirmDialog.tsx` — чисто клиентское подтверждение для деструктивных
действий, не серверный канал). Единый бэкенд-путь закрывает оба режима
потребления (headless JSON и веб-чат) одновременно.

### 3.1 — Sentinel-тип и guidance-текст
- `internal/agent/` (рядом с `peak_hours_stop.go`, по тому же паттерну):
  `AwaitingAnswerError` (аналог `PeakHoursError`), `AwaitingAnswerGuidance
  (question, sessionID string) string` (аналог `PeakHoursGuidance`) — текст
  уже сформулирован в этой сессии, с готовой командой резюме
  `rush run --role <role> --session <id> "<answer>"`.
- Wiring в `coordinator.go`/`agent.go`'s `AddFinish`-путь — форсирует
  завершение хода, как peak-hours stop.

### 3.2 — Новый tool + `exit_reason`
- Новый agent tool (`ask_question` или аналог) в `internal/agent/tools/` —
  принимает вопрос + опциональный список вариантов ответа, вызывает
  `AwaitingAnswerError`.
- `internal/cmd/run.go` — новое значение `exit_reason: "awaiting_answer"`
  в JSON-конверте, отдельное от `error`/`cancelled` (аналог того, как уже
  обрабатывается `peak_hours`).

### 3.3 — Веб-UI (опциональная косметика, НЕ блокер для 3.1/3.2)
- Рендер `options[]` из tool-вызова как quick-reply чипов над полем ввода
  в `web/src/components/Chat.tsx` — клик по чипу отправляет тот же текст,
  что обычное сообщение. Никакой новой WebSocket/pubsub инфраструктуры.
- «Закрыть без ответа» не требует отдельного состояния — пользователь
  просто не отвечает, чат продолжает жить как обычно.

### 3.4 — Документация
- `internal/cmd/claude_slash_command.md` — новый абзац: как оркестратору
  реагировать на `exit_reason == "awaiting_answer"` (прочитать вопрос,
  ответить самому/спросить оператора, возобновить `--session <id>`).

### 3.5 — Тесты
- `AwaitingAnswerError`/`AwaitingAnswerGuidance` — юнит-тесты по образцу
  `peak_hours_stop_test.go`.
- `exit_reason` в JSON-конверте — интеграционный тест по образцу
  существующих `run_complete_test.go`/аналогов.
- Сам tool — юнит-тест на форсированное завершение хода.

## Порядок выполнения

1. Коммит правок `CLAUDE.md` (отдельно, до всего остального).
2. Фаза 1 — параллельно, независимые файлы, каждый через отдельного
   агента с zero-trust верификацией оркестратором.
3. Фаза 3.1+3.2 (core question tool) — можно параллельно с Фазой 1,
   независимые файлы.
4. Фаза 2 — последовательно, после Фазы 1 (учиться на мелких успехах
   перед owned-зонами). Пункт про MCP OAuth (2.4) — только по отдельному
   да пользователя.
5. Фаза 3.3/3.4/3.5 — после 3.1/3.2.
