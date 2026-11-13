# Финальный спринт «агентская финализация batch-агентов» (S4–S7): план параллельной доставки

- Дата: 2026-11-13. F0 = этот документ; коммитит оркестратор.
- Назначение: единственный источник, по которому несколько агентов работают ПАРАЛЛЕЛЬНО в разных git-worktree, не ломая друг друга. Владение файлами, швы, порядок слияния и политика тестов фиксируются здесь и меняются только новой редакцией документа.
- Источники истины: код и тесты main (все `file:symbol` ниже проверены по коду ветки `final-plan-20261113`); PR #389 (S0+S1, merged; раздел «Still to come (not in this PR)»); `docs/plans/2026-10-21-h4-execution-manifest.md` и `docs/plans/2026-10-21-h4-execution-manifest-lanes.md` — прецедент lane-декомпозиции, только идеи; worktree `wrush-finalization-runtime-95-20261110` (WIP, НЕ принят, read-only) — только идеи, не импорт вслепую.
- Примечание: в теле PR #389 на GitHub дословного заголовка «Still required before readiness» нет — соответствующий перечень остаётся в разделе «Still to come (not in this PR)» тела PR и в п. 3 этого документа (вопрос к оператору зафиксирован в §12).

> **Редакция 2 (оркестратор, по решениям оператора) — приоритетнее текста ниже при расхождении.**
> - **Доставка.** Полосы НЕ идут в CI и в main по отдельности. Везде ниже, где сказано «PR полосы» или «независимые PR», читать: «ветка полосы». Оркестратор создаёт интеграционную ветку `feat/finalization-sprint-20261113` (от `origin/main` + этот документ), вливает в неё F1 первым, затем каждую принятую полосу, затем W1/W2. В main идёт ОДИН PR из интеграционной ветки после ОДНОГО большого полного fork CI в конце.
> - **Тесты.** Rush-агенты тесты не запускают вообще (они вешают машину). Оркестратор сам запускает тесты серийно и локально: `D:/dev/go/crush/.rush/tmp/golock.sh go test -p 1 -parallel 1 -count=1 <пакеты полосы>`, только затронутые пакеты, по одному запуску за раз, в worktree полосы. Агент проверяет только `gofumpt`, `go build`, `go vet` изменённых пакетов через тот же `golock.sh` (абсолютный путь выше, в worktree скрипта нет).
> - **Одновременность агентов.** Не более 4–5 rush-агентов одновременно (лимиты провайдера); их go-команды сериализует `golock.sh`.
> - **Ответы на §12 (приняты оркестратором как проектные умолчания).** (1) Состав hold'ов для L2 — host-локи + tracked process trees + batch root leases; если агент найдёт ещё один durable-источник, он заявляет его в отчёте, а не молча добавляет. (2) Запрет правок `sdk/sdk.go` и `internal/server/hub.go` с общим choke на уровне `internal/app` — утверждён. (3) Имя флага `options.finalization.enabled` — утверждено. (4) Сводка п. 3 — правильная. (5) web/-статус «ожидает печати» в этом спринте НЕ делаем.
> - **Исправление контракта §5.1.** Композитный ридер строится ПО ВЛАДЕЛЬЦУ: интерфейс `AgentFinalizationScopeReader.FinalizationSealScopeFacts(ctx, tx)` не получает owner, поэтому конструктор принимает его первым аргументом: `NewAgentFinalizationScopeReader(ownerSessionID string, descendants, queued, holds, debt)`; пустой `ownerSessionID` даёт `Complete=false` (fail-closed).

## 1. Цель

Без финализации («тормозов») родитель может объявить «готово», пока его дети ещё работают или остаются долги реакций. Финализация: финальное сообщение batch-агента сначала — провизорный ЧЕРНОВИК (кандидат); он становится окончательным («запечатывается») только когда дети завершены, долги реакций погашены, нет отмены и нет открытых мутаций; иначе агент ждёт и пересобирает ответ. Клиенты видят «готово» только после запечатывания.

Спринт закрывает готовность (readiness) фичи: последняя миля поверх уже слитых и инертных слоёв S0–S3, плюс opt-in-выключатель и проводка. Решение «включать по умолчанию» — отдельный шаг оператора (W3), не часть спринта.

## 2. Что уже в main (S0–S3) и какие инварианты держит

Всё перечисленное уже в main и ИНЕРТНО: в production не существует ни одного вызова `agent.withFinalizationExecution` (только тесты), поэтому `finalizationExecutionFor` (`internal/agent/finalization_execution.go:34`) возвращает nil, а все точки вызова в turn-конвейере просто пропускаются (`ts.finalization == nil`). Также `BindAgentFinalization` не вызывается из production; единственный вызов `WithAgentFinalizationAuthorities` (`internal/agent/batch_install_adapters.go:809`) передаёт ноль authorities — это легаси no-op-binding по контракту `internal/session/finalization_authority.go:70`.

| Слой | file:symbol | Инвариант |
|---|---|---|
| S0 gate | `internal/agent/finalization_gate.go`: `finalizationGate`, `newFinalizationGate`, `admitMutation`, `cancel`, `beginCandidate`, `trySeal`, `waitState`, `signalLocked` | владелец/эпоха/serial/revision; мутация допускается ДО side effect и держит запись видимой; канал `changed` — сигнал изменения; `trySeal` публикует только при тихих фактах и нулевых мутациях |
| S0 identity/tickets | `internal/session/agent_finalization.go`: `AgentFinalizationIdentity`, `Bind`, `Snapshot`, `Admit`, `Release`, `SealTx`, `advanceAgentFinalization` | доверенная идентичность, не ввод модели; ticket до работы; повторное завершение released-тикета невозможно; revision — high-water; `SealTx` — conditional UPDATE по revision+hash+serial, отказ = 0 записей |
| S0 batch admission | `internal/session/finalization_mutation.go`: `admitFinalizationMutation`; `internal/session/finalization_ancestors.go`: `readFinalizationParentEdges`, `readFinalizationAncestry` | productive batch-запись резервируется под финализационных owners до эффекта |
| S1 fencing | `internal/session/batch_finalization_tx.go`: `validateBatchFinalizationTx` | каждый продуктивный writer-tx перепроверяет ancestry+admission до эффекта; «нет записи» — наблюдение внутри tx, никогда не proof-of-admission |
| S1 candidates (message) | `internal/message/finalization.go`: `FinalizationIdentity`, `MarkFinalizationCandidate`, `AcceptFinalizationCandidate`, `VerifyFinalizationCandidate`, `FinalizationContentHash`, `Finish.FinalizationIdentity` | провизорный Partial-footer на `Finish`; иммутабельный хэш stored-parts; акцепт только под атомарным seal; повторный акцепт детерминирован (`ErrFinalizationAlreadyAccepted`) |
| S1 tx-хранилище | `internal/message/finalization_tx.go`: `FinalizationReader`, `FinalizationWriter`, `GetFinalizationCandidateTx`, `StoreFinalizationCandidateTx` | кандидат читается/пишется в writer-tx вызывающего; checkpoint-generation fence; 0 строк = changed, не успех |
| S1 step stamps | `internal/agent/finalization_execution.go`: `beginFinalizationStep`, `markFinalizationStep`, `persistFinalizationCandidate`, `isFinalizationCandidate`; точки: `internal/agent/agent_turn_step.go:216`, `:658-659`, `:827-828` | клеймо кандидата на каждом provider-шаге, изоляция по сессии и логическому вызову, checkpoint fence против старого partial-писателя |
| S2 persist/recover | `internal/session/finalization_candidate.go`: `StoreCandidateTx`, `RecoverCandidate`, `AgentFinalizationRecovery`, `candidateIntact`; `internal/agent/finalization_candidate.go`: `persistFinalizationCandidate`, `recoverFinalizationCandidate`, `resumeCandidateSerial` | сообщение+запись в одном writer-tx; восстановление кандидата после рестарта (adopt/fail); serial high-water переживает очистку; поднятый serial никогда не опускается |
| S2 publish/seal | `internal/agent/finalization_publish.go`: `publishFinalizationCandidate`; `internal/agent/finalization_seal.go`: `sealFinalizationCandidate`; `internal/session/agent_finalization.go`: `SealTx` | публикация только после commit; оба durable-приёмника (record + message) должны согласиться; «not yet» — детерминированный отказ, не latch failure |
| S3 TrySeal | `internal/session/finalization_seal.go`: `TrySeal`, `AgentFinalizationSealReason` (+13 типизированных причин), `AgentFinalizationSealOutcome`, `AgentFinalizationScopeFacts`, `AgentFinalizationScopeReader`, `FinalizationSealWriter` | фиксированный порядок проверок внутри одного tx; nil-ридер или `Complete=false` — отказ `scope_facts_unestablished` (fail-closed); отмена, открытые tickets, revision, descendants/debt/holds/queued — отказы до записи |
| S3 durable queue | `internal/session/finalization_queue.go`, `finalization_queue_prepare.go`: `AgentFinalizationQueuePreparer`, `PrepareAgentFinalizationQueue`, `Consume`; `finalization_queue_restore.go`: `AgentFinalizationQueueService`, `RestoreAgentFinalizationQueue`; `finalization_queue_lease.go`: `bindFinalizationQueueLeaseTx`, `ErrAgentFinalizationLeaseChanged`; `finalization_queue_enqueue.go`: `enqueueFinalizationAware`; `finalization_queue_failure.go`: `recordFinalizationQueueFailure`; `internal/agent/finalization_queue_reservation.go`: `FinalizationQueueCoordinator`, `runFinalizationQueuedCall`, `finalizationQueueDriverForRun`; `internal/agent/finalization_queue_call.go`: `restoreFinalizationQueuedCall`, `selectFinalizationQueueDriver`, `enqueueFinalizationCall` | durable-очередь вызовов владельца: payload-hash, lease-токен, идемпотентное потребление, restore только по доверенному snapshot |
| authorities | `internal/session/finalization_authority.go`: `AgentFinalizationService`, `AgentFinalizationAuthority`, `BindAgentFinalization`, `WithAgentFinalizationAuthorities`, `revalidate`; `internal/session/finalization_context.go`: `AgentFinalizationContext`, `CaptureAgentFinalizationContext`, `Restore`, `RestoreCall`, `RequiresManagedDriver`; `internal/session/finalization_tx.go`: `FinalizationTransaction`, `WithFinalizationTx` | неизменяемая capability, привязанная к сервису; наследование authority только по проверенным ребрам; транспорт идентичности не меняет frozen policy/creds/tools/budgets |
| binding | `internal/agent/finalization_driver_binding.go`: `finalizationLogicalIdentity`, `bindFinalizationIdentity`, `bindFinalizationDriverCall`; `internal/agent/finalization_call_context.go`: `restoreFinalizationCallContext` | привязка scope при старте драйвера по доверенной capability; конфликт идентичностей = отказ |
| storage | миграции `internal/db/migrations/20261109000002_agent_finalization.sql` (`session_agent_finalizations`, `session_agent_finalization_tickets`), `20261110000003_agent_finalization_continuations.sql` (`session_agent_finalization_continuations`), `20261112000001_agent_finalization_candidate_serial.sql` (`candidate_serial`); `batch_nodes` — в `20261022000001_store_v1_initial.sql`; тестовая схема `internal/db/dbtest/finalization_schema.go`: `FinalizationFencingSchema`, `ApplyFinalizationFencingSchema` (+ дрифт-тест равенства с миграциями) | частичная уникальность активной инкарнации на владельца; каскады удаления |

Существующие production-точки вызова (уже nil-safe; до W1 не трогать никем):
- `internal/agent/agent_turn.go:50` — `restoreFinalizationCallContext`; `:596` — `ts.fenceCheckpoint = checkpoint.fenceFinalization`.
- `internal/agent/agent_turn_step.go:216` — `beginFinalizationStep`; `:658-659` — `markFinalizationStep`; `:827-828` — `persistFinalizationCandidate`.
- `internal/agent/agent_run.go:43` — `restoreFinalizationCallContext`; `:708` — `bindFinalizationDriverCall`.
- `internal/agent/coordinator_run_queue_call.go:43` — `restoreFinalizationQueuedCall`; `:346` — `finalizationAuthorities`; `:427` — `finalizationQueueDriverForRun`.
- `internal/agent/coordinator_interrupt.go:843` — `enqueueFinalizationCall` (interrupt → durable-очередь).

## 3. Чего не хватает (readiness; сводка из PR #389)

1. Привязать авторитетное логическое исполнение и покрыть КАЖДОГО продуктивного производителя: потомки, физические/персистентные hold'ы, долги уведомлений/реакций, очередная работа.
2. Durable cross-process admission/revision/seal safety — локального mutex недостаточно.
3. `wait` и `compose` через тот же driver/mailbox с сохранением creds, роли модели, tools, бюджетов.
4. Отложить разрешение очереди и каждое терминальное уведомление CLI/SDK/HTTP/WS/batch до принятой публикации.
5. Детерминированные интеграционные регрессии и доказательства merge/post-main.

Закрытие 1–2 — фактами внутри `TrySeal` (L1–L3) + композитный `AgentFinalizationScopeReader` (F1/W1). Закрытие 3 — цикл wait/compose (L5) на существующем driver (queue-путь уже доказывает сохранение контекста: `runFinalizationQueuedCall` + `RebuildSessionAgentCall`). Закрытие 4 — шлюз `FinalizationTerminalGate` (F1) + L7/L8. Закрытие 5 — W2 + политика тестов §7.

## 4. Декомпозиция и порядок слияния

F0 (этот документ) → **F1** (вливается ПЕРВЫМ) → **L1…L9** (параллельно, в любом порядке, каждый в своём worktree и PR) → **W1** → **W2** → **W3** (решение оператора, вне PR).

Принципы (жёсткие):
- каждая полоса пишет ТОЛЬКО свои новые файлы; исключение ровно одно: L9 добавляет одну строку-поле в `Options` (`internal/config/config_mcp.go:246`);
- общие точки входа подключаются ТОЛЬКО через швы из F1; вставки в существующие общие файлы делает только W1 (правки посчитаны в §4.2);
- каждая полоса ИНЕРТНА: её production-код не имеет вызовов до W1; unit-тесты полосы вызывают её функции напрямую;
- sqlc НЕ регенерируется в спринте; tx-уровневые запросы — инлайн-SQL в файлах полосы (прецеденты: `internal/session/batch_api_store.go:18`, `internal/session/batch_child_bind.go:82`, `validateBatchFinalizationTx`);
- новых миграций НЕ нужно (все таблицы есть, §2); потребность в изменении схемы — стоп и эскалация оператору, не самодеятельность;
- полосы не трогают замороженные файлы S0–S3 (список в §4.2) и не редактируют файлы размером ≥ 950 строк, кроме разрешённых правок W1.

### 4.1 Полосы и их файлы (полное владение)

**F1 — «швы и тест-кит»** (маленький PR, без поведения):
- `internal/session/finalization_facts.go` — интерфейсы источников фактов + композитный ридер (контракт §5.1);
- `internal/session/finalization_debt_seam.go` — интерфейс погашения долгов (§5.2);
- `internal/session/finalization_notify_gate.go` — шлюз терминальных уведомлений + инертный дефолт + polling-реализация (§5.3);
- `internal/agent/finalization_hooks.go` — хуки драйвера с fail-closed/инертными дефолтами (§5.4);
- `internal/session/finalization_testkit_test.go` — фикстуры настоящих производителей: bind владельца (`BindAgentFinalization`), живой потомок (child-запись `session_agent_finalizations` через `Bind` + `CreateTaskSession`), очередной вызов (`EnqueueRunQueueEntry` + continuation), долг (`InsertSessionNotice`), отмена (`RequestCancel`); схема через `dbtest.ApplyFinalizationFencingSchema`;
- `internal/agent/finalization_testkit_test.go` — фикстура связанного драйвера: bind + создание `finalizationExecution` (`withFinalizationExecution`) + композитный ридер (расширяет паттерны `finalization_driver_context_test.go`, `finalization_install_test.go`).

**L1 — факты о потомках** (`internal/session/finalization_facts_descendants.go`, `finalization_facts_descendants_test.go`): реализует `FinalizationDescendantsFacts` (§5.1). Источники в tx: активные записи `session_agent_finalizations` дочерних сессий владельца (по `sessions.parent_session_id`), живые узлы `batch_nodes` (`owner_session_id`), живая работа дерева (`GetCallTreeActivity` как семантический референс — `internal/session/session_read.go:56`). Инвариант: считает ТОЛЬКО активные фазы (`executing|waiting_work|composing`).

**L2 — факты об очереди и hold'ах** (`internal/session/finalization_facts_queue.go`, `finalization_facts_holds.go`, `+ _test.go`): реализует `FinalizationQueuedCallsFacts` и `FinalizationPhysicalHoldsFacts`. Очередь: незавершённые `RunQueueEntry` владельца (`HasOutstandingRunQueueEntriesForSession` как референс — `internal/session/session_runqueue.go:573`) + неконсумированные строки `session_agent_finalization_continuations`. Hold'ы (точный список источников оператор подтверждает — §12): durable host-локи (`internal/session/host_store.go`, `host_lock.go`), tracked process trees (`internal/session/track_windows.go`: `TrackProcessTree`), batch root leases (`internal/agent/batch_install_adapters.go:770`). Все счётчики — в tx `TrySeal`.

**L3 — долги реакций/уведомлений, уровень session** (`internal/session/finalization_debt.go`, `finalization_debt_settle.go`, `+ _test.go`): реализует `FinalizationReactionDebtFacts` и `AgentFinalizationDebtSettler`. Источник долга: session-notice'ы (`internal/session/session_notice_store.go`, производитель — `internal/agent/coordinator_reaction_chain.go:60`). Погашение — идемпотентное, с durable-квитанцией в стиле `Release` (`internal/session/agent_finalization.go:165`): двойной settle не понижает счётчик дважды.

**L4 — привязка scope при старте драйвера + восстановление после рестарта** (`internal/agent/finalization_bind.go`, `finalization_recover.go`, `+ _test.go`): реализует хуки `BindScope`/`RecoverScope` (§5.4). Bind использует существующие `bindFinalizationDriverCall`/`bindFinalizationIdentity` (точка уже стоит: `internal/agent/agent_run.go:708`). Recover вызывает `recoverFinalizationCandidate` (`internal/agent/finalization_candidate.go:61`) и `resumeCandidateSerial` — поднимает serial выше durable high-water (`Record.CandidateSerial`), маппит `AgentFinalizationRecovery` (adopt/failed/none) на вердикты цикла L5.

**L5 — ожидание потомков + пересборка + погашение долгов драйвером** (`internal/agent/finalization_wait.go`, `finalization_recompose.go`, `finalization_settle.go`, `+ _test.go`): реализует хук `FinalizeTurn` (§5.4, контракт цикла §5.5). Wait — через `gate.waitState`; compose — пересборка тем же драйвером (свежий stamp, новый кандидат; сохранение creds/role/tools/budgets гарантируется самой архитектурой driver/mailbox — см. очередь, §2); settle — через `AgentFinalizationDebtSettler` перед повторным `trySeal`. Референс формы цикла (идея, не импорт): worktree `wrush-finalization-runtime-95-20261110`, `internal/agent/finalization_lifecycle.go:63` (`finalizeCandidate`).

**L6 — отмена/прерывание** (`internal/agent/finalization_cancel.go`, `+ _test.go`): реализует хук `OnCancel`: `gate.cancel()` + пометка record-пути к отказу; интеграция с durable-очередью уже есть (`coordinator_interrupt.go:843`). В `internal/agent/coordinator_interrupt.go` (~950 строк) полоса НЕ пишет ни строки — точку вставки делает W1 (≤6 строк).

**L7 — уведомления CLI/SDK** (`internal/cmd/run_finalization.go`, `+ _test.go`; `sdk/finalization.go`, `+ _test.go`): обёртки «терминальный вывод/`RunResult` не раньше `AllowTerminal`/`AwaitSealed`» (§5.3). `sdk/sdk.go` (999 строк) НЕ редактируется: обёртка подключается на уровне `internal/app` (W1) и в новом `sdk/finalization.go`; флаг OFF — поведение байт-в-байт прежнее.

**L8 — уведомления HTTP/WS/batch-результаты** (`internal/server/finalization_notify.go`, `+ _test.go`; `internal/session/finalization_notify.go`, `+ _test.go`): фильтрует/откладывает терминальные события до запечатывания. Каналы событий: `internal/server/api_events_feeder.go:43,54` (`app.Messages.Subscribe`, `app.Sessions.Subscribe`), `internal/server/api_events.go:108` (`APIEventLog.Subscribe`), WS-хаб `internal/server/hub.go`. `hub.go` (992 строки) НЕ редактируется. Статус «ожидает печати» для web/ — НЕ в этом спринте по умолчанию; если полоса считает его необходимым, это отдельное согласование в описании PR (§12).

**L9 — выключатель** (единственный владелец общих файлов): новый `internal/config/finalization_flag.go` (+ `_test.go`): тип `FinalizationOptions` и аксессор (§6); РОВНО одна добавленная строка в `Options` (`internal/config/config_mcp.go:246`); регенерация корневого `schema.json` командой `rush schema` (`internal/cmd/schema.go`); обновления: `README.md`, новый `docs/agent-finalization.md`, `internal/skills/builtin/rush-config/SKILL.md`, упоминание флага в help/slash-шаблонах (`internal/cmd/claude_slash_command.yaml`, `internal/cmd/claude_wrush_command.md`, `internal/cmd/claude_wcrush_command.md`).

**W1 — проводка** (один агент, единственный трогающий общие точки): флаг → привязка → читатели → цикл ожидания → шлюзы. Точки: (1) `internal/agent/agent_run.go` (909 строк): ≤12 строк в районе `:43`/`:708` — при флаге ON и bound-requirement построить `finalizationExecution` через `withFinalizationExecution` и установить хуки; (2) `internal/agent/agent_turn.go` (794): ≤8 строк в конце turn'а — вызов `FinalizeTurn`; (3) `internal/agent/coordinator_interrupt.go` (950): ≤6 строк у `:843` — `OnCancel`; (4) установка композитного ридера и шлюзов при конструкции App — новый `internal/app/app_finalization_install.go` + одна строка вызова в точке сборки агента; (5) `internal/cmd/run.go` (450): ≤10 строк при необходимости. Если какая-то правка может пересечь 1000 строк в файле — сначала pure-move split по правилу fork-гайда (оракул: отсортированный список `^(func|type|var|const) ` до/после — diff обязан быть пустым), затем правка.

**W2 — сквозные регрессии + живая проверка** (новые тест-файлы, например `internal/app/finalization_e2e_test.go`): детерминированные сценарии «родитель ждёт → дети завершаются → долги гасятся → seal → готово», отмена, рестарт, двойная публикация; живая проверка с включённым флагом ТОЛЬКО на изолированных `RUSH_GLOBAL_DATA` И `RUSH_GLOBAL_CONFIG` (обе переменные — см. §8), либо с project-local `rush.json`.

**W3** — решение оператора о включении по умолчанию; вне спринта, отдельная запись.

### 4.2 Таблица владения и конфликтов (существующие файлы)

Размеры проверены (`wc -l`). «—» = файл в спринте не меняется.

| Файл | Строк | Кто меняет | Ограничение |
|---|---|---|---|
| `internal/agent/agent.go` | 966 | — | НЕ редактировать (структурные поля не нужны: контекст несёт `finalizationExecution`) |
| `internal/server/hub.go` | 992 | — | НЕ редактировать |
| `sdk/sdk.go` | 999 | — | НЕ редактировать (обёртка в `sdk/finalization.go` + choke на уровне `internal/app`) |
| `internal/agent/coordinator_interrupt.go` | 950 | W1 | ≤6 строк, район `:843` |
| `internal/agent/agent_turn_step.go` | 932 | — | НЕ редактировать: все три точки (`:216`, `:658-659`, `:827-828`) уже стоят и nil-safe |
| `internal/agent/agent_run.go` | 909 | W1 | ≤12 строк, районы `:43`, `:708` |
| `internal/agent/agent_turn.go` | 794 | W1 | ≤8 строк, конец turn'а |
| `internal/agent/coordinator.go` | 714 | W1 (при необходимости) | минимально |
| `internal/agent/coordinator_run_queue_call.go` | 574 | — | точки уже стоят (`:43`, `:346`, `:427`) |
| `internal/cmd/run.go` | 450 | W1 | ≤10 строк при необходимости |
| `internal/config/config_mcp.go` | 564 | L9 | ровно одна строка-поле в `Options` (`:246`) |
| `internal/config/config.go` | 691 | — | НЕ редактировать |
| `schema.json` (корень) | — | L9 | регенерация `rush schema` после правки тегов |
| `README.md`, `docs/agent-finalization.md` (новый), `internal/skills/builtin/rush-config/SKILL.md`, `internal/cmd/claude_slash_command.yaml`, `internal/cmd/claude_wrush_command.md`, `internal/cmd/claude_wcrush_command.md` | — | L9 | только упоминание флага |
| `internal/app/*` (точка сборки) | — | W1 | новый `internal/app/app_finalization_install.go` + одна строка вызова |
| `internal/session/finalization_seal.go`, `finalization_candidate.go`, `agent_finalization.go`, `finalization_authority.go`, `finalization_context.go`, `finalization_tx.go`, `finalization_mutation.go`, `finalization_ancestors.go`, `finalization_ingress_helpers.go`, `finalization_queue*.go`, `internal/message/finalization*.go`, `internal/agent/finalization_gate.go`, `finalization_driver_binding.go`, `finalization_execution.go`, `finalization_publish.go`, `finalization_seal.go`, `finalization_candidate.go`, `finalization_call_context.go`, `finalization_queue_reservation.go`, `finalization_queue_call.go` | — | — | ЗАМОРОЖЕНЫ (S0–S3): ни одна полоса не редактирует; ошибка в них = баг-репорт оператору, не локальный фикс |
| `internal/agent/batch_install_adapters.go` | 850+ | — | НЕ редактировать (вызов `:809` — легаси no-op, остаётся) |

Замечания по пересечениям (уточнение относительно исходной декомпозиции оператора):
- `sdk/sdk.go` — 999 строк и `internal/server/hub.go` — 992 строки: любое добавление ломает страж 1000. Поэтому L7/L8 пишут только новые файлы, а подключение идёт через choke `internal/app` (W1) — единственная точка, где терминальное завершение приложения задерживается до seal; CLI/SDK/WS наследуют это. Прямые правки `sdk.go`/`hub.go` запрещены всем, включая W1.
- `agent_turn_step.go` не входит ни в одну полосу (в исходном брифе предполагались правки ~922 строк; фактически 932, и правки не нужны — точки уже стоят).
- `coordinator_interrupt.go` — только W1, минимальная точка; L6 — только свои файлы.
- L3↔L5: L5 потребляет интерфейс `AgentFinalizationDebtSettler` из F1, L3 его реализует — компиляционной зависимости полос друг от друга нет.
- L9 единственный владелец общих документационных/конфиговых файлов спринта.

### 4.3 Зависимости

| Полоса | Зависит от | Почему |
|---|---|---|
| F1 | main (S0–S3) | типы-приёмники: `AgentFinalizationScopeReader`, `AgentFinalizationStore`, `finalizationGate` |
| L1 | F1 | реализует `FinalizationDescendantsFacts` |
| L2 | F1 | реализует `FinalizationQueuedCallsFacts`, `FinalizationPhysicalHoldsFacts` |
| L3 | F1 | реализует `FinalizationReactionDebtFacts`, `AgentFinalizationDebtSettler` |
| L4 | F1 | реализует `BindScope`, `RecoverScope` |
| L5 | F1 | реализует `FinalizeTurn`; потребляет facts-интерфейсы и `AgentFinalizationDebtSettler` только как интерфейсы |
| L6 | F1 | реализует `OnCancel` |
| L7 | F1 | потребляет `FinalizationTerminalGate` |
| L8 | F1 | потребляет `FinalizationTerminalGate` |
| L9 | main | только конфиг и документация |
| W1 | L1–L9 (все слиты) | устанавливает реализации в швы |
| W2 | W1 | сквозные проверки |

## 5. Контракты швов F1

F1 объявляет ТОЛЬКО типы/интерфейсы/дефолты. Имена ниже — обязательные (на них ссылаются полосы и W1). Сигнатуры в стиле репозитория: `context.Context` первым, `error` последним (staticcheck ST1008). Дефолты: fail-closed там, где решение о публикации; инертные no-op там, где решается «делать ли legacy-действие».

### 5.1 Источники фактов — `internal/session/finalization_facts.go`

```go
package session

// Каждый источник читает свои факты ВНУТРИ writer-транзакции TrySeal.
// Реализации — полосы L1/L2/L3; счётчик = число активных объектов владельца.
type FinalizationDescendantsFacts interface {
    FinalizationActiveDescendantsTx(ctx context.Context, tx *sql.Tx, ownerSessionID string) (int, error)
}

type FinalizationQueuedCallsFacts interface {
    FinalizationQueuedCallsTx(ctx context.Context, tx *sql.Tx, ownerSessionID string) (int, error)
}

type FinalizationPhysicalHoldsFacts interface {
    FinalizationPhysicalHoldsTx(ctx context.Context, tx *sql.Tx, ownerSessionID string) (int, error)
}

type FinalizationReactionDebtFacts interface {
    FinalizationReactionDebtTx(ctx context.Context, tx *sql.Tx, ownerSessionID string) (int, error)
}

// NewAgentFinalizationScopeReader собирает единственный production-ридер
// AgentFinalizationScopeReader. Любой nil-член даёт факты с Complete=false:
// TrySeal отказывает с AgentFinalizationSealScopeIncomplete (fail-closed),
// и агент уходит в wait/compose, а не в публикацию. Ошибка члена пробрасывается
// как ошибка чтения фактов (TrySeal вернёт error) — это состояние «ридер сломан».
func NewAgentFinalizationScopeReader(
    ownerSessionID string, // Редакция 2: ридер строится по владельцу; пустой = Complete=false.
    descendants FinalizationDescendantsFacts,
    queued FinalizationQueuedCallsFacts,
    holds FinalizationPhysicalHoldsFacts,
    debt FinalizationReactionDebtFacts,
) AgentFinalizationScopeReader
```

Семантика для полос L1–L3: `ownerSessionID` — владелец record'а; незнакомый владелец (нет ни одного объекта) = 0, НЕ ошибка; ошибка БД — ошибка (TrySeal не запечатает); факты читаются тем же `tx`, что и seal-решение (см. `TestTrySealScopeReaderReadsInsideTransaction` в `internal/session/finalization_seal_test.go:479`).

### 5.2 Погашение долгов — `internal/session/finalization_debt_seam.go`

```go
package session

// AgentFinalizationDebtSettler идемпотентно погашает долги реакций/уведомлений
// владельца. Реализация — L3; потребитель — L5 (settle перед повторным trySeal).
// Возвращает число погашенных записей; повторный вызов возвращает 0.
type AgentFinalizationDebtSettler interface {
    SettleFinalizationDebts(ctx context.Context, ownerSessionID string) (int, error)
}
```

### 5.3 Шлюз терминальных уведомлений — `internal/session/finalization_notify_gate.go`

```go
package session

// FinalizationTerminalGate решает, можно ли доставлять терминальное уведомление
// (CLI-финал, SDK RunResult, HTTP/WS session-ended, batch-результат).
// Инертность по построению: НЕТ активной финализационной записи = разрешено,
// поэтому при флаге OFF (записи не создаются) шлюз ведёт себя как no-op.
type FinalizationTerminalGate interface {
    // AllowTerminal: false только когда у владельца активная запись
    // (phase in executing|waiting_work|composing). accepted / нет записи /
    // failed|cancelled|interrupted = true (доставляем обычный терминал).
    AllowTerminal(ctx context.Context, ownerSessionID string) bool

    // AwaitSealed блокирует до выхода записи из активной фазы. nil = можно
    // доставлять. Ошибки: ctx.Err() или ошибка чтения store.
    AwaitSealed(ctx context.Context, ownerSessionID string) error
}

// Инертный дефолт: всё разрешено, AwaitSealed = немедленный nil.
func DefaultFinalizationTerminalGate() FinalizationTerminalGate

// Durable-реализация на polling: in-process канал не видит чужие процессы; используется W1.
func NewPollingFinalizationTerminalGate(store *AgentFinalizationStore, poll time.Duration) FinalizationTerminalGate
```

### 5.4 Хуки драйвера — `internal/agent/finalization_hooks.go`

```go
package agent

// FinalizationDriverHooks — единственные точки, куда W1 подключает L4/L5/L6.
// Дефолты (zero value): все хуки nil = инертное legacy-поведение.
// W1 обязан при флаге ON устанавливать ВСЕ хуки: установка неполного набора —
// ошибка установки (fail-closed), а не тихий no-op.
type FinalizationDriverHooks struct {
    // BindScope при старте драйвера до первого шага (L4). nil = no-op (инертно).
    BindScope func(context.Context, SessionAgentCall) error
    // RecoverScope при старте/рестарте: RecoverCandidate + resumeCandidateSerial (L4).
    // nil = пропустить восстановление (инертно; допустимо только при флаге OFF).
    RecoverScope func(context.Context, *finalizationExecution) (session.AgentFinalizationRecovery, error)
    // FinalizeTurn — цикл wait/compose/seal в конце turn'а (L5). nil = no-op.
    FinalizeTurn func(context.Context, *turnStream) error
    // OnCancel — точка отмены из coordinator interrupt (L6). nil = no-op.
    OnCancel func(sessionID string)
}
```

### 5.5 Контракт цикла wait/compose (реализует L5; потребляет швы, а не полосы)

Наблюдаемое поведение `FinalizeTurn`:
1. Прочитать факты: `gate.waitState` (существующий API `finalization_gate.go:229`). `accepted` — вернуть успех (запечатано; публикация уже внутри `trySeal` → `sealFinalizationCandidate` → `session.AgentFinalizationStore.TrySeal`).
2. `finalizationPauseQuestion` — вернуть удержанный вопрос как `AwaitingAnswerError` (референс: WIP-worktree `finalization_lifecycle.go:60`).
3. `finalizationWaitWork`: погасить долги (`AgentFinalizationDebtSettler`), затем ждать сигнал `changed` ИЛИ poll-таймер durable-фактов ИЛИ `ctx.Done()`/`cancel_requested`. Обязателен poll-таймер: изменения из ДРУГИХ процессов (дети в других worktree/процессах) не видны in-process каналу; интервал = интервал supervision check-in'а.
4. `finalizationCompose`: пересобрать ответ тем же драйвером/mailbox (creds/role/tools/budgets сохраняются самой архитектурой driver-вызова; свежий stamp через `beginCandidate`; новый кандидат через `persistFinalizationCandidate`), затем снова `trySeal`.
5. Ограничение зависания: каждое ожидание ограничено ctx/отменой/supervision; бесконечного молчаливого цикла нет — состояние видно (см. §9).

## 6. Политика opt-in

- Флаг: `options.finalization.enabled` (bool, по умолчанию **OFF**). Тип `FinalizationOptions` — в новом `internal/config/finalization_flag.go`; поле `Finalization *FinalizationOptions json:"finalization,omitempty" jsonschema:"description=Agent finalization (batch agents): hold the final answer until children finish, reaction debts settle, no cancellation and no open mutations; default=false"` — единственная правка L9 в `internal/config/config_mcp.go:246`. Аксессор: `func (o *Options) AgentFinalizationEnabled() bool` (nil-safe, false по умолчанию).
- Что включает: ровно список установок W1 (§4.1): построение `finalizationExecution` и хуков при bound-requirement, композитный ридер фактов, шлюзы уведомлений, цикл `FinalizeTurn`, `OnCancel`. При OFF поведение байт-в-байт совпадает с сегодняшним main (проверка: `finalizationExecutionFor` возвращает nil; ни одного production-вызова швов).
- Откат: выключить флаг (config reload/файл). Обратной миграции не нужно: sealed-записи остаются валидными принятыми сообщениями; continuation-строки уже обработаны; legacy-путь их игнорирует.
- Решение «ON по умолчанию» — W3, отдельное решение оператора после W2; в этом спринте default не меняется.
- Изоляция любой живой проверки: `RUSH_GLOBAL_DATA` И `RUSH_GLOBAL_CONFIG` — оба в throwaway-каталоги (см. §8), либо project-local `rush.json`.

## 7. Политика тестов

- Агенты пишут код И тесты в файлах своей полосы. Тесты — детерминированные, без сети, с `t.Parallel()` где возможно, `t.TempDir()` для временных каталогов, testify `require`.
- Агент проверяет сборку и vet ТОЛЬКО изменённых пакетов через golock — сериализующий скрипт go-команд оператора (в корневом checkout это `.rush/tmp/golock.sh`; в worktree его нет — точная команда указана в задании полосы). Пример: `golock go build ./internal/session/... ./internal/agent/...`, `golock go vet ./internal/session/...`.
- `go test` агенты НЕ запускают. Тесты по одному, серийно, запускает оркестратор в worktree агента (golock, `go test -p 1 -parallel 1 -count=1 ./<pkg>`; тяжёлые — под capm 2g), затем fork CI. Причины: параллельные `go test -race` на этой машине падают по памяти (`ERROR_COMMITMENT_LIMIT`), а голый полный прогон ломал сам рантайм Go.
- Агент сдаёт со своим PR: (а) точный список имён тестов (`Test...`); (б) revert-check для каждого теста — какую мутацию в production-коде тест ловит и как убедиться, что тест падает при её внесении (контракт `/wcrush`: реверты выполняет оркестратор на фазе 2).
- Полный `go test ./...`, голый `go build ./...` и `golangci-lint run` по всему дереву — запрещены полосам.
- Golden-файлы: полоса, меняющая вывод, попадающий в golden, обязана заявить это в PR (оркестратор решает; `-update` не запускать без разрешения).

## 8. Репозиторные стражи (на них уже спотыкались — соблюдать с первого раза)

1. **platform.Command вместо exec.Command/CommandContext** — все новые запуски процессов только через `internal/platform` (`internal/platform/command.go`); страж `TestNoUnhardenedProcessSpawns` (`internal/platform/spawn_guard_test.go`) сканирует код на прямые вызовы. Особенно актуально L7/L8/W2.
2. **cobra-хендлеры в тестах требуют `SetContext`** — паттерн: `rootCmd.SetContext(context.Background())` (`internal/cmd/lock_test.go:307-311`; пояснение в `internal/cmd/models_bump_test.go:34`). Актуально для L7.
3. **Линтеры**: gofumpt (форматирование — `task fmt` / `gofumpt -w`), `noctx` (запуск процессов без ctx), staticcheck ST1008 (error — последний возвращаемый). Логи с заглавной буквы; комментарии на своих строках — с заглавной и с точкой.
4. **Файлы ≤ 1000 строк** — `.githooks/check_file_size.sh` (pre-push) + ratchet `.githooks/file_size_allowlist.txt` + revive `file-length-limit` (max 1000, `skipComments:false`, `skipBlankLines:false`). Опасные файлы перечислены в §4.2; новые файлы полос не должны приближаться к лимиту.
5. **schema.json синхронна с тегами** — после любого изменения json/jsonschema-тегов конфига: `rush schema > schema.json` (`internal/cmd/schema.go`); дрифт = падение проверки схемы. Актуально только L9.
6. **Никакой работы с реальным конфигом машины**: `--cwd` изолирует ТОЛЬКО workspace-scope. Для global-scope обязательно BOTH `RUSH_GLOBAL_DATA` и `RUSH_GLOBAL_CONFIG` в throwaway-каталоги одной конкретной команды; лучше — project-local `rush.json`. Проверяй реальный глобальный конфиг после «изолированных» запусков, не верь прошлым проверкам.
7. **Никаких git-записей от полос** (commit/push и т.д.) — коммитит оркестратор; полосы оставляют рабочее дерево с изменениями.
8. **Не раздувать чужие файлы** — см. §4.2 (замороженные файлы и лимиты правок W1).

## 9. Риски и чем закрыты

| Риск | Закрытие |
|---|---|
| Зависание ожидания (ребёнок/hold не завершаются; cross-process изменения не видны in-process каналу) | цикл L5 ждёт на трёх источниках: сигнал gate + poll durable-фактов (интервал supervision) + ctx; терминальная запись о состоянии (см. риск «тихий цикл»); отмена через `OnCancel`/`RequestCancel` всегда выводит из ожидания (`TrySeal` отказ `cancel_requested`) |
| «Тихий» бесконечный цикл пересборки (готово не наступает и никто не видит почему) | типизированные причины отказа (`AgentFinalizationSealReason.String()`) пробрасываются в статус ожидания; L8 публикует состояние ожидания в терминальный канал только после seal/отказа; supervision check-in видит повторяющиеся compose; наблюдаемость — критерий приёмки L5/W2 |
| Двойная публикация | уже в main: `TrySeal` идемпотентен (`Accepted`), `AcceptFinalizationCandidate` возвращает `ErrFinalizationAlreadyAccepted`, `publishFinalizationCandidate` требует согласия обоих приёмников; W2 добавляет детерминированный регресс «seal после seal» |
| Бесконечная пересборка (recompose loop) | revision/serial — монотонные high-water (`advanceAgentFinalization`, `resumeCandidateSerial`); compose допускается только со свежим stamp; stale stamp → compose ровно один раз на шаг; тест L5: повторный compose со старым stamp запрещён |
| Гонки отмены/рестарта | `TrySeal`/`SealTx`/`StoreCandidateTx` проверяют `cancel_requested` и identity-фенсинг ВНУТРИ tx; generation/node fencing (`readAgentFinalization`); durable-очередь с lease (`finalization_queue_lease.go`) и handoff при interrupt (`coordinator_interrupt.go:843`, тесты `finalization_queue_cancel_handoff_test.go` уже в main); L6 покрывает: cancel во время wait/compose/seal |
| Потеря долга (settle упал после частичного погашения; край гибели процесса между seal и уведомлением) | долги — durable session-notice'ы; settle идемпотентен с квитанцией (§5.2); счётчик долга читается в tx seal (незакрытый долг = отказ seal); тест L3: crash между частями settle → повтор converges |
| Локальный mutex вместо cross-process safety | все решения — в SQLite-транзакциях с conditional UPDATE; tickets durable; polling-шлюз вместо in-process chan; W2: два «процесса» (два соединения) compete за seal — ровно один выигрывает |
| Инертность сломалась (полоса случайно начала влиять на production) | приёмка каждой полосы включает «grep-proof»: `git grep -n "<новые символы>" -- ":(exclude)полоса"` показывает только шовные файлы и тесты; оркестратор повторяет проверку на каждом merge |

## 10. Критерии приёмки (наблюдаемые) и порядок доставки

Порядок доставки: F1 → L1…L9 (любой порядок, независимые PR) → W1 → W2 → W3 (решение оператора). Полоса считается принятой, когда оркестратор НЕЗАВИСИМО проверил пункты её столбца ниже.

| Полоса | Наблюдаемые критерии приёмки |
|---|---|
| F1 | только новые файлы (git status не показывает правок существующих); интерфейсы компилируются; unit-тесты дефолтов: nil-члены композита → `Complete=false`; `DefaultFinalizationTerminalGate` разрешает всё; zero-хуки = no-op; grep-proof: ни один шов не вызывается из production |
| L1 | счётчик потомков растёт/падает ровно по активным записям; тесты на «нет потомков» = 0; чтение строго в переданном tx; тест «чужой tx» не смешивает данные |
| L2 | очередь: незавершённые entry+continuation считаются, потреблённые нет; holds: каждый заявленный источник покрыт тестом; nil/пустой владелец = 0 |
| L3 | reader видит открытый долг; settle идемпотентен (второй вызов = 0); crash-тест: повтор после сбоя сходится; счётчик в tx согласован с reader |
| L4 | bind по доверенной capability переносит authorities в драйвер (реиспользует `bindFinalizationIdentity`); recover: adopted/failed/none маппятся; serial после рестарта ≥ durable `CandidateSerial` |
| L5 | сценарии: тихо→seal; ребенок активен→wait→(ребёнок завершился)→seal; heldQuestion→pause; долг→settle→seal; отмена→выход; poll-таймер видит внешнее изменение; stale stamp→один compose; состояние ожидания наблюдаемо (причина отказа доступна) |
| L6 | cancel до seal → отказ `AgentFinalizationSealCancelled`, доставки нет; cancel во время wait выводит цикл; interrupt→очередь (существующий handoff) не ломается; после cancel повторный seal невозможен |
| L7 | флаг OFF: вывод CLI/`RunResult` байт-в-байт прежний (golden/диф-тест); флаг ON (unit): терминал задержан до `AllowTerminal`/`AwaitSealed`; `sdk/sdk.go` не изменён (git diff пуст) |
| L8 | терминальные события задержаны до seal; `hub.go` не изменён; batch-результат не отдаётся раньше seal; статус «ожидает печати» НЕ добавляется в web/ без отдельного согласования |
| L9 | `AgentFinalizationEnabled()` nil-safe=false; `schema.json` перегенерирован и валиден; docs/skill/help/slash упоминания согласованы; конфиг без флага валиден |
| W1 | флаг OFF: все тесты main зелёные, поведение прежнее; флаг ON (in-process e2e): родитель не завершён, пока ребёнок работает; после завершения ребёнка и погашения долга — seal, затем «готово»; неполная установка хуков = ошибка установки; правки общих файлов в пределах норм §4.2 |
| W2 | детерминированные регрессии (wait/cancel/restart/double-seal/debt-crash) зелёные под golock `-p 1 -parallel 1`; живая проверка с флагом на изолированных `RUSH_GLOBAL_DATA`+`RUSH_GLOBAL_CONFIG` (или project-local rush.json); реальный глобальный конфиг машины не изменился (проверка после) |
| W3 | запись оператора: default OFF/ON; при ON — отдельная смена дефолта и документация |

## 11. Вне спринта (P3 backlog S3 — не переносить в полосы)

- typed-nil messages (аномальные нулевые `message.Message` значения на отказных путях);
- контракт доверия ридера фактов (формальное доказательство, что источники L1–L3 не лгут внутри tx);
- proof-of-absence production-вызова через grep (механизация «ни один production-вызов не включает финализацию» как CI-проверка);
- статус «ожидает печати» в web/;
- смена default флага (W3 отдельно);
- любые новые миграции/sqlc-регенераты.

## 12. Открытые вопросы к оператору (не блокируют F1; ответы фиксировать здесь новой редакцией)

1. Точный состав «физических/персистентных hold'ов» для L2: host-локи + tracked process trees + batch root leases — достаточно? Что-то ещё считается durable-удержанием?
2. Подтверждение запрета правок `sdk/sdk.go` (999) и `internal/server/hub.go` (992) с choke на уровне `internal/app` (§4.2) — приемлемо ли, что CLI/SDK/WS задерживаются одной общей точкой?
3. Название флага: `options.finalization.enabled` — подтвердить или предложить иное.
4. «Still required before readiness»: в теле PR #389 дословно такого раздела нет (есть «Still to come (not in this PR)»); перечень п. 3 этого документа — правильная сводка?
5. L8: нужен ли в спринте минимальный web/-статус «ожидает печати», или дефолт «без web/» утверждён?
