# Changelog

## [1.9.1](https://github.com/sky-ai-eng/triage-factory/compare/v1.9.0...v1.9.1) (2026-05-08)

### Bug Fixes

* **github:** per-job log fallback when jobs still running ([#127](https://github.com/sky-ai-eng/triage-factory/issues/127)) ([0ebbdef](https://github.com/sky-ai-eng/triage-factory/commit/0ebbdefe28f6a9d6fc79f0086a627cfcded0c462))
* **jira-cli:** make `edit` flag parsing reject ambiguous and trailing flags ([b0edbd1](https://github.com/sky-ai-eng/triage-factory/commit/b0edbd19628d24bca17e65f4f736e50c04685782))

## [1.9.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.8.1...v1.9.0) (2026-05-07)


### Features

* **delegate:** use per-prompt allowed-tools from skills ([#122](https://github.com/sky-ai-eng/triage-factory/issues/122)) ([4c79cf8](https://github.com/sky-ai-eng/triage-factory/commit/4c79cf87cee5dd453f0bc6e9f6a0bdd6783fa1d3))
* **tracker:** add review_request_removed event and reconcile tasks ([#112](https://github.com/sky-ai-eng/triage-factory/issues/112)) ([fef901f](https://github.com/sky-ai-eng/triage-factory/commit/fef901fe75c1738c7bfded7971429cad9c481db4))

## [1.8.1](https://github.com/sky-ai-eng/triage-factory/compare/v1.8.0...v1.8.1) (2026-05-07)


### Bug Fixes

* **backfill:** default all checkboxes off, split sections ([#120](https://github.com/sky-ai-eng/triage-factory/issues/120)) ([acaa696](https://github.com/sky-ai-eng/triage-factory/commit/acaa696210467e0c25b611f42e6cadeca925b638))

## [1.8.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.7.1...v1.8.0) (2026-05-07)


### Features

* **classify:** per-project quorum classifier on entity discovery ([#117](https://github.com/sky-ai-eng/triage-factory/issues/117)) ([342db5c](https://github.com/sky-ai-eng/triage-factory/commit/342db5cd4fb94acceb42c8a4932a5b9358f1df20))
* **delegate:** consolidate scratch dirs + propagate project knowledge ([#116](https://github.com/sky-ai-eng/triage-factory/issues/116)) ([2c1572f](https://github.com/sky-ai-eng/triage-factory/commit/2c1572fbedc68110a809b8f7ab463ee2d188a0a8))
* **projects:** backfill popup on project create + import ([#118](https://github.com/sky-ai-eng/triage-factory/issues/118)) ([c1d30ed](https://github.com/sky-ai-eng/triage-factory/commit/c1d30ede3bdffe7f5a361156dd29976312c42c06))
* **projects:** entities panel under knowledge base on project detail ([#119](https://github.com/sky-ai-eng/triage-factory/issues/119)) ([4b7f615](https://github.com/sky-ai-eng/triage-factory/commit/4b7f6157591d166b62231a94c7ed87c753433bda))


### Bug Fixes

* **uninstall:** drop curator Claude sessions + sync clean-slate.sh ([#114](https://github.com/sky-ai-eng/triage-factory/issues/114)) ([07826bb](https://github.com/sky-ai-eng/triage-factory/commit/07826bb2519083d0f6297bb7c9eca83f76c6f20b))

## [1.7.1](https://github.com/sky-ai-eng/triage-factory/compare/v1.7.0...v1.7.1) (2026-05-06)


### Bug Fixes

* Use creds overlay for settings page ([#110](https://github.com/sky-ai-eng/triage-factory/issues/110)) ([a4eac22](https://github.com/sky-ai-eng/triage-factory/commit/a4eac2239dc1c167c31ebcb211986ded5dce08b3))

## [1.7.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.6.1...v1.7.0) (2026-05-06)


### Features

* **auth:** support TRIAGE_FACTORY_* env vars as credential source ([#105](https://github.com/sky-ai-eng/triage-factory/issues/105)) ([1186013](https://github.com/sky-ai-eng/triage-factory/commit/1186013c0e78649ea8f9a32ab9d0e4dd079cae7d))
* **github:** add SSH/HTTPS toggle for bare-clone setup ([#109](https://github.com/sky-ai-eng/triage-factory/issues/109)) ([873aad3](https://github.com/sky-ai-eng/triage-factory/commit/873aad3455d68d3c2bd3be61e2d8307e66bb7520))


### Performance Improvements

* **test:** cache schema bundle for in-memory test DBs ([#107](https://github.com/sky-ai-eng/triage-factory/issues/107)) ([1fe8696](https://github.com/sky-ai-eng/triage-factory/commit/1fe8696916114fca46419ab79890cfba4ae16a7f))

## [1.6.1](https://github.com/sky-ai-eng/triage-factory/compare/v1.6.0...v1.6.1) (2026-05-06)


### Bug Fixes

* **agentproc:** bufio.Scanner tool_result lines don't kill runs ([#103](https://github.com/sky-ai-eng/triage-factory/issues/103)) ([d6fda3b](https://github.com/sky-ai-eng/triage-factory/commit/d6fda3b3146a62fcfd30227b86a4ba60fb8bd620))
* **config:** settings in SQLite not yaml, default poll to 5m ([#100](https://github.com/sky-ai-eng/triage-factory/issues/100)) ([c65a5b5](https://github.com/sky-ai-eng/triage-factory/commit/c65a5b55489b96728d66a25f8c32250dd9978ce3))
* **factory:** flush turn-pole belt seams via analytic endpoint tangents ([#104](https://github.com/sky-ai-eng/triage-factory/issues/104)) ([9a49369](https://github.com/sky-ai-eng/triage-factory/commit/9a49369ea4e46b07e249737902b167c1832835a8))
* **review:** multi-line comments must be within one diff hunk ([#101](https://github.com/sky-ai-eng/triage-factory/issues/101)) ([96b989d](https://github.com/sky-ai-eng/triage-factory/commit/96b989d38c78ec00b985c6d4e1260a1d4db0a5e3))

## [1.6.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.5.0...v1.6.0) (2026-05-05)


### Features

* **curator:** add built-in Jira formatting skill ([#96](https://github.com/sky-ai-eng/triage-factory/issues/96)) ([31dc26f](https://github.com/sky-ai-eng/triage-factory/commit/31dc26f13579894d12ddfc88d6692258f40c3b38))
* **delegate:** lazy Jira worktrees + pending-PR approval flow ([#98](https://github.com/sky-ai-eng/triage-factory/issues/98)) ([88d75fc](https://github.com/sky-ai-eng/triage-factory/commit/88d75fcdf8d08082cb1f191065ab87c798f271ba))


### Bug Fixes

* **prompts:** add prompt version migration ([#99](https://github.com/sky-ai-eng/triage-factory/issues/99)) ([63f1700](https://github.com/sky-ai-eng/triage-factory/commit/63f1700b5ccfe47d2d7fc860c7603114d866fa04))

## [1.5.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.4.0...v1.5.0) (2026-05-04)


### Features

* **curator:** chat panel + live KB watcher + reset ([#92](https://github.com/sky-ai-eng/triage-factory/issues/92)) ([9bfcbb3](https://github.com/sky-ai-eng/triage-factory/commit/9bfcbb3d1bba2e9aa016228566f50485f47cba14))
* **curator:** envelope + hidden context-change channel ([#90](https://github.com/sky-ai-eng/triage-factory/issues/90)) ([8523521](https://github.com/sky-ai-eng/triage-factory/commit/852352144de78639151ab85a9875a462c42ec5bd))
* **curator:** per-project Claude Code chat sessions ([#87](https://github.com/sky-ai-eng/triage-factory/issues/87)) ([1e9fb2e](https://github.com/sky-ai-eng/triage-factory/commit/1e9fb2e6916e2b7249ad111ad5f5612018595b7d))
* **curator:** per-project ticket-spec skill ([#94](https://github.com/sky-ai-eng/triage-factory/issues/94)) ([ea1e617](https://github.com/sky-ai-eng/triage-factory/commit/ea1e617d5997dd52d67aa68f4148f5b59ff78d82))
* **delegate:** yield-to-user pause/resume for agents (SKY-139) ([#84](https://github.com/sky-ai-eng/triage-factory/issues/84)) ([82e8dc8](https://github.com/sky-ai-eng/triage-factory/commit/82e8dc81680e90bea0705ab92c23eabffb1e9171))
* **projects:** /projects page + tracker links + knowledge sidebar ([#89](https://github.com/sky-ai-eng/triage-factory/issues/89)) ([5f1d4a1](https://github.com/sky-ai-eng/triage-factory/commit/5f1d4a1b3f7a57508986d47e21d81c6372ac4fb0))
* **projects:** add SKY-222 project export/import bundles ([#91](https://github.com/sky-ai-eng/triage-factory/issues/91)) ([25744f5](https://github.com/sky-ai-eng/triage-factory/commit/25744f50c005d164881d9d08b0021b010fd6e05e))
* **projects:** schema + CRUD API (SKY-215) ([#85](https://github.com/sky-ai-eng/triage-factory/issues/85)) ([62bbe02](https://github.com/sky-ai-eng/triage-factory/commit/62bbe020083bc481c138bb697c72584d4b38a120))
* **worktree:** bootstrap bare clones + PR refspec + origin URL repair (SKY-214) ([#82](https://github.com/sky-ai-eng/triage-factory/issues/82)) ([b9e1bd4](https://github.com/sky-ai-eng/triage-factory/commit/b9e1bd4f3290825aa871ee93493c74ba09d3e003))


### Bug Fixes

* **curator:** repo materialization + shared allowlist + git -C support ([#88](https://github.com/sky-ai-eng/triage-factory/issues/88)) ([688785b](https://github.com/sky-ai-eng/triage-factory/commit/688785b3caa60cd86ee0de0f60d99f195138e45a))
* **delegate:** extract agentproc package ([#86](https://github.com/sky-ai-eng/triage-factory/issues/86)) ([e8b3468](https://github.com/sky-ai-eng/triage-factory/commit/e8b346844a6b681241c6d5cc9454092895dd1d46))
* **tracker:** updatedAt-gated PR refresh for monorepo scale ([#93](https://github.com/sky-ai-eng/triage-factory/issues/93)) ([d6f5d2d](https://github.com/sky-ai-eng/triage-factory/commit/d6f5d2d18abeeb3144b26b04f390ea5c301a2483))

## [1.4.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.3.0...v1.4.0) (2026-05-02)


### Features

* **board:** drag AgentCards between columns in terminal run states ([#77](https://github.com/sky-ai-eng/triage-factory/issues/77)) ([71a6d3a](https://github.com/sky-ai-eng/triage-factory/commit/71a6d3aeea74b8e3931c328c94ae113964228009))
* **reviews:** block second submit-review per run + clearer queue wording (SKY-212) ([#78](https://github.com/sky-ai-eng/triage-factory/issues/78)) ([cb02370](https://github.com/sky-ai-eng/triage-factory/commit/cb0237028970ac9b0f428b3b64831e000efd2038))
* **reviews:** cleanup discarded reviews + split /undo from /requeue (SKY-206) ([#75](https://github.com/sky-ai-eng/triage-factory/issues/75)) ([4c5a8b1](https://github.com/sky-ai-eng/triage-factory/commit/4c5a8b1ebafedb1e415f611dc170b63da41e7348))
* **reviews:** persist human verdict to run_memory.human_content (SKY-205) ([#74](https://github.com/sky-ai-eng/triage-factory/issues/74)) ([f5b8512](https://github.com/sky-ai-eng/triage-factory/commit/f5b851279823589a7c858600f1aaaeb6d693a908))
* **reviews:** Return-to-queue button on pending_approval AgentCards (SKY-207) ([#76](https://github.com/sky-ai-eng/triage-factory/issues/76)) ([b522644](https://github.com/sky-ai-eng/triage-factory/commit/b522644dfce370e66b473e0688939730b42ff0fe))


### Bug Fixes

* **delegate:** stamp run-message timestamps before WS broadcast (SKY-213) ([#79](https://github.com/sky-ai-eng/triage-factory/issues/79)) ([1be816c](https://github.com/sky-ai-eng/triage-factory/commit/1be816cb011c79671c03e70164321051e5053139))
* **factory:** drag-to-delegate from station drawer ([#70](https://github.com/sky-ai-eng/triage-factory/issues/70)) ([1ece04d](https://github.com/sky-ai-eng/triage-factory/commit/1ece04d1185fb3e4f34c3edbd92ec43e446b14a5))
* **reviews:** expand pr-review focus + drive inline suggestions ([#81](https://github.com/sky-ai-eng/triage-factory/issues/81)) ([263e846](https://github.com/sky-ai-eng/triage-factory/commit/263e846a1513967ef1fa6aa503d5f020581b48ba))

## [1.3.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.2.1...v1.3.0) (2026-05-01)


### Features

* **factory:** SKY-196 – move to 3d factory view ([#58](https://github.com/sky-ai-eng/triage-factory/issues/58)) ([480d5e6](https://github.com/sky-ai-eng/triage-factory/commit/480d5e6dd0d0b4c43508fec83c761a4977a4b49a))


### Bug Fixes

* **factory:** animate terminal states before closure ([#69](https://github.com/sky-ai-eng/triage-factory/issues/69)) ([07d778c](https://github.com/sky-ai-eng/triage-factory/commit/07d778c5510f5da18f6d548aabd87fa136c9023c))

## [1.2.1](https://github.com/sky-ai-eng/triage-factory/compare/v1.2.0...v1.2.1) (2026-04-30)


### Bug Fixes

* **ci:** set CLA bot required inputs ([489ac92](https://github.com/sky-ai-eng/triage-factory/commit/489ac9247d5742584b2d65e0b4f23be752f24c11))
* **github:** intercept 406 on large PR diffs in start-review ([#64](https://github.com/sky-ai-eng/triage-factory/issues/64)) ([92807ff](https://github.com/sky-ai-eng/triage-factory/commit/92807ff2bcb911a3280a3261084268665a94ec33))
* **tracker:** strip trailing slash from Jira base URL ([#59](https://github.com/sky-ai-eng/triage-factory/issues/59)) ([21394ae](https://github.com/sky-ai-eng/triage-factory/commit/21394ae4ff0ee2be19ed5999ab0fee0a63f30228))

## [1.2.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.1.2...v1.2.0) (2026-04-27)


### Features

* **tracker:** real source times for label / review-request / ready-for-review events ([#56](https://github.com/sky-ai-eng/triage-factory/issues/56)) ([38f1845](https://github.com/sky-ai-eng/triage-factory/commit/38f1845fc2bcc433e67d5a8af0a2957f1c3775e0))

## [1.1.2](https://github.com/sky-ai-eng/triage-factory/compare/v1.1.1...v1.1.2) (2026-04-27)


### Bug Fixes

* **db:** serialize writes so contention queues in Go rather than racing ([#54](https://github.com/sky-ai-eng/triage-factory/issues/54)) ([0da8941](https://github.com/sky-ai-eng/triage-factory/commit/0da894141644debcf1bb93e93cc7324d1af0256a))

## [1.1.1](https://github.com/sky-ai-eng/triage-factory/compare/v1.1.0...v1.1.1) (2026-04-27)


### Bug Fixes

* **deps:** fix npm audit ([d58be7d](https://github.com/sky-ai-eng/triage-factory/commit/d58be7de46cd3aabc482dc1971e11bb253f0befb))

## [1.1.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.0.0...v1.1.0) (2026-04-27)


### Features

* **cmd:** add uninstall command ([#52](https://github.com/sky-ai-eng/triage-factory/issues/52)) ([3128709](https://github.com/sky-ai-eng/triage-factory/commit/3128709a7ccc180ec5319c4606551d27976e6114))


### Bug Fixes

* **ci:** drop component prefix from release-please tags ([#49](https://github.com/sky-ai-eng/triage-factory/issues/49)) ([2ac3bf9](https://github.com/sky-ai-eng/triage-factory/commit/2ac3bf95970a41193c5a6ccc4846fa864fb54af9))
* **prompts:** deduplicate skill search dirs and skills themselves ([#51](https://github.com/sky-ai-eng/triage-factory/issues/51)) ([2832b92](https://github.com/sky-ai-eng/triage-factory/commit/2832b9294c012e1b5da4192ab0d5ea55cfc611bc))

## 1.0.0 (2026-04-27)


### Features

* add codeowners ([92a9e1a](https://github.com/sky-ai-eng/triage-factory/commit/92a9e1a528777a4939805c46c53c3ded1c961bdf))
* add configurable jira tags to watch for ([4e2106d](https://github.com/sky-ai-eng/triage-factory/commit/4e2106d7a33abe8787e60f1769a5a2584c172633))
* **board:** allow agent delegated tasks to move around ([0a32914](https://github.com/sky-ai-eng/triage-factory/commit/0a329141cdd4396b9bb8a40c5cdf00b661a2520e))
* **carry-over:** auto-prefill + available-to-claim bucket ([#39](https://github.com/sky-ai-eng/triage-factory/issues/39)) ([9954666](https://github.com/sky-ai-eng/triage-factory/commit/99546667a5bcf439cbb6e3d7263ce54f7934da44))
* **ci:** release pipeline + required CI checks + pure-Go SQLite ([#47](https://github.com/sky-ai-eng/triage-factory/issues/47)) ([f1bd3d9](https://github.com/sky-ai-eng/triage-factory/commit/f1bd3d969c6703fc7f846ecebdc742f78e1cc355))
* **db:** add prompt_triggers table for automated delegation ([#15](https://github.com/sky-ai-eng/triage-factory/issues/15)) ([2252ab1](https://github.com/sky-ai-eng/triage-factory/commit/2252ab1ca76d64e44264bcbe34401db592d18d26))
* **db:** rewrite schema for entity-first per-action event model (SKY-175) ([#20](https://github.com/sky-ai-eng/triage-factory/issues/20)) ([58405e7](https://github.com/sky-ai-eng/triage-factory/commit/58405e7ac8687013d3d90168956421f841b76cb0))
* **delegate:** add prompt selection support to backend ([3f41287](https://github.com/sky-ai-eng/triage-factory/commit/3f4128713f1d612f0f8a9465da1e1215bc1f5cff))
* **delegate:** add prompt stats view ([2170207](https://github.com/sky-ai-eng/triage-factory/commit/21702074b4d46a342a85f04cc9970d45e0ddc855))
* **delegate:** added prompts page ([49c17aa](https://github.com/sky-ai-eng/triage-factory/commit/49c17aa21675a5bddb73d59803331328805a94f9))
* **delegate:** added skills-as-prompts importer ([2641d9d](https://github.com/sky-ai-eng/triage-factory/commit/2641d9dc4e0848435dcbb055d67db8eddc4d5844))
* **delegate:** adjust PR review bot to include pricing information ([44cccc7](https://github.com/sky-ai-eng/triage-factory/commit/44cccc79980fbfc25f20db9a428c0ba7752334f8))
* **delegate:** event-driven auto-delegation with safety gates ([#17](https://github.com/sky-ai-eng/triage-factory/issues/17)) ([0b88d29](https://github.com/sky-ai-eng/triage-factory/commit/0b88d290dffa9dbb00e3c6e1aa32333a5b9c0fac))
* **delegate:** generalize envelope and delegation ([#5](https://github.com/sky-ai-eng/triage-factory/issues/5)) ([f09bfd6](https://github.com/sky-ai-eng/triage-factory/commit/f09bfd64d5d575c16a1889224f27397900de7360))
* **delegate:** graph wiring for default prompts ([ff8aa81](https://github.com/sky-ai-eng/triage-factory/commit/ff8aa8161a40b6a893569bc61f94cff810842905))
* **delegate:** persist task-specific memory ([#13](https://github.com/sky-ai-eng/triage-factory/issues/13)) ([9113885](https://github.com/sky-ai-eng/triage-factory/commit/9113885d594f0f25fa8da6433e17e2d78cce81c3))
* **delegate:** real prompt placeholders + workflow_run_id + list-runs (SKY-194) ([#42](https://github.com/sky-ai-eng/triage-factory/issues/42)) ([4fbdf34](https://github.com/sky-ai-eng/triage-factory/commit/4fbdf3461a4c91421c4969531956edd73614ba59))
* **delegate:** remove 'default' concept and just use 'auto' ([#18](https://github.com/sky-ai-eng/triage-factory/issues/18)) ([e34f504](https://github.com/sky-ai-eng/triage-factory/commit/e34f5044c1fc9e1394369d0a828572791eaab972))
* **delegate:** task_unsolvable status, circuit breaker, and global kill switch ([#16](https://github.com/sky-ai-eng/triage-factory/issues/16)) ([c697b41](https://github.com/sky-ai-eng/triage-factory/commit/c697b41ed0a7a55a84e351445a51d2ee8b1c171b))
* **docs:** add CLAUDE.md ([c088145](https://github.com/sky-ai-eng/triage-factory/commit/c088145659844f131f766607643d426f00d74dc7))
* **docs:** update repo documentation ([8ba5c4a](https://github.com/sky-ai-eng/triage-factory/commit/8ba5c4ae788dbc5b2fdd2ee198f71ed255531d8b))
* entity-first data model rewrite (SKY-174) ([c045f71](https://github.com/sky-ai-eng/triage-factory/commit/c045f71c630af58a2e832db2f1e74bcb46ceec4e))
* **events:** add full event and predicate registry ([db18f50](https://github.com/sky-ai-eng/triage-factory/commit/db18f509dab93c31929ed34a01ad981f98a8b7ea))
* **events:** add status predicate to jira assigned/available ([6d9a86b](https://github.com/sky-ai-eng/triage-factory/commit/6d9a86b4bd87b18e282b892e0ea35214cfb20c6b))
* **factory:** delegations now queue linearly based on events ([#45](https://github.com/sky-ai-eng/triage-factory/issues/45)) ([c170fd5](https://github.com/sky-ai-eng/triage-factory/commit/c170fd565a84b553ad0a470743792281cb4ccb09))
* **factory:** initial pass with dummy data ([#43](https://github.com/sky-ai-eng/triage-factory/issues/43)) ([b9546dd](https://github.com/sky-ai-eng/triage-factory/commit/b9546dd400c71675026b99d3fadf4ca8ec146a3e))
* **frontend:** overhaul dashboard and jira signup flow ([#7](https://github.com/sky-ai-eng/triage-factory/issues/7)) ([e66b754](https://github.com/sky-ai-eng/triage-factory/commit/e66b754d5646e6006f1e1ceace1a2900c4d70a04))
* **frontend:** trigger config panel on Prompts page (SKY-186) ([#27](https://github.com/sky-ai-eng/triage-factory/issues/27)) ([6d535c7](https://github.com/sky-ai-eng/triage-factory/commit/6d535c7282928788bb6aabd9c24e1e73228ba946))
* **gh:** `exec gh actions download-logs` + unified repo resolution ([#12](https://github.com/sky-ai-eng/triage-factory/issues/12)) ([75bbc6b](https://github.com/sky-ai-eng/triage-factory/commit/75bbc6bfbd5e47fcff7f87b0358c66259ef5cb3e))
* **github:** add PR tracking page ([120bf74](https://github.com/sky-ai-eng/triage-factory/commit/120bf7455ab63a02084de7f6527cd06561736209))
* **github:** added PR stats ([28df348](https://github.com/sky-ai-eng/triage-factory/commit/28df34872f61a826f9e4a130578402a798e26f8d))
* **github:** draggable draft state ([f98747f](https://github.com/sky-ai-eng/triage-factory/commit/f98747fc242b6c4b71b782e1d066492913c8cdac))
* **github:** emit `github_pr_new_commits` event ([#10](https://github.com/sky-ai-eng/triage-factory/issues/10)) ([202cbfa](https://github.com/sky-ai-eng/triage-factory/commit/202cbfaa8626a1eacdb215566e4ee30adf8dc139))
* **github:** track granular CI state for all PRs ([#11](https://github.com/sky-ai-eng/triage-factory/issues/11)) ([0d3e3a6](https://github.com/sky-ai-eng/triage-factory/commit/0d3e3a678431a6c61e15324ee497590e7dc3d30e))
* initial commit ([e8f83c7](https://github.com/sky-ai-eng/triage-factory/commit/e8f83c73ba12d5cc2f4eb9e9ed5687e8ba6ec8e7))
* **integrations:** reload pollers when keychain values change ([0cd2835](https://github.com/sky-ai-eng/triage-factory/commit/0cd2835d679f6d06ddb3115d73f97ffb9b6a3aa1))
* **jira:** add jira CLI shim ([34fc575](https://github.com/sky-ai-eng/triage-factory/commit/34fc575065a95efaf997ffb5f326586ebd738051))
* **jira:** add jira CLI shim ([#2](https://github.com/sky-ai-eng/triage-factory/issues/2)) ([34fc575](https://github.com/sky-ai-eng/triage-factory/commit/34fc575065a95efaf997ffb5f326586ebd738051))
* **jira:** claim guards for multi-task entities (SKY-183) ([#28](https://github.com/sky-ai-eng/triage-factory/issues/28)) ([378e626](https://github.com/sky-ai-eng/triage-factory/commit/378e626d2c49a3ad5d69149ac3e9c9100571bc41))
* **jira:** skip task creation for parents with open subtasks (SKY-173) ([#38](https://github.com/sky-ai-eng/triage-factory/issues/38)) ([fdef74f](https://github.com/sky-ai-eng/triage-factory/commit/fdef74f9b393bef08c82e1e7bb56279c63088448))
* **jira:** status rules — read sets vs canonical write (SKY-192) ([#32](https://github.com/sky-ai-eng/triage-factory/issues/32)) ([be7c0f5](https://github.com/sky-ai-eng/triage-factory/commit/be7c0f56bf4c5015e76c667732ebb4d8a607ad42))
* persist PRs and split tracking into a discover + diff solution ([#6](https://github.com/sky-ai-eng/triage-factory/issues/6)) ([05a4252](https://github.com/sky-ai-eng/triage-factory/commit/05a42526d20d8f709b04043ef6e4c265964a74ae))
* **prs:** subscribe to WS events for real-time updates (SKY-151) ([#37](https://github.com/sky-ai-eng/triage-factory/issues/37)) ([e95a260](https://github.com/sky-ai-eng/triage-factory/commit/e95a2606774a3a03d3b2fdc9bc137726c6348add))
* **repo:** rename the whole thang ([#19](https://github.com/sky-ai-eng/triage-factory/issues/19)) ([1466645](https://github.com/sky-ai-eng/triage-factory/commit/14666450aa9368f594560ae9d7b96387360d39b1))
* repos as first-class entities + profiling + system redesign ([#4](https://github.com/sky-ai-eng/triage-factory/issues/4)) ([149d65c](https://github.com/sky-ai-eng/triage-factory/commit/149d65ca3b80012be01c437173d8976f8b17d53e))
* **repos:** link present doc chips to the file on GitHub ([06a6d69](https://github.com/sky-ai-eng/triage-factory/commit/06a6d691b7c3f3c059db2d20ec1425a9190f8b4c))
* **repos:** redesign page with liquid-glass horizontal bands ([e3fef19](https://github.com/sky-ai-eng/triage-factory/commit/e3fef19073d90eb070e4f813977dcf368a0c8e47))
* **repos:** redesign page with liquid-glass horizontal bands ([b218c9c](https://github.com/sky-ai-eng/triage-factory/commit/b218c9cca99a6ec1183343b0ad3749b56a3e02f9))
* **review:** add db helpers ([5223439](https://github.com/sky-ai-eng/triage-factory/commit/522343915ae6a710edf3f7306795e6ecfb7856aa))
* **review:** add refractor syntax highlighting ([3b87073](https://github.com/sky-ai-eng/triage-factory/commit/3b870730760639c0027f4786a2cad6bf354b8c1a))
* **review:** add required CRUD routes for approval display ([163832a](https://github.com/sky-ai-eng/triage-factory/commit/163832a55b5730bff7a7b03afccf9f3a1450c452))
* **review:** add review components and dependencies ([ce16b18](https://github.com/sky-ai-eng/triage-factory/commit/ce16b1867f056bf0af2c547ce123bd3411d396ef))
* **review:** API endpoints for review approval flow ([93375ac](https://github.com/sky-ai-eng/triage-factory/commit/93375acfa62908e7297407bc4713e20bc41273a0))
* **review:** gate submit-review to defer when TODOTINDER_REVIEW_PREVIEW=1 ([8e57093](https://github.com/sky-ai-eng/triage-factory/commit/8e570939782d1188074700b39d39b36e41e85968))
* **review:** PR reviews await user approval instead of auto-posting ([d5bebe1](https://github.com/sky-ai-eng/triage-factory/commit/d5bebe15223f6a5e1c055909ef2902be94f0705d))
* **review:** set TODOTINDER_REVIEW_PREVIEW=1 env var in spawner ([939d4c3](https://github.com/sky-ai-eng/triage-factory/commit/939d4c37ad54ab61fe17f7205dca21847aedf043))
* **review:** show review summary in markdown or raw text ([66dc809](https://github.com/sky-ai-eng/triage-factory/commit/66dc80951f86ac8f4a60ab330f29492982157bb6))
* **routing:** autonomy-suitability gate + post-scoring re-derive (SKY-181, SKY-182) ([#26](https://github.com/sky-ai-eng/triage-factory/issues/26)) ([bef4d74](https://github.com/sky-ai-eng/triage-factory/commit/bef4d74e5755e47e17e9acacc1a807d75cc1dd12))
* **routing:** entity-first event pipeline (SKY-177 + SKY-178 + SKY-179) ([#23](https://github.com/sky-ai-eng/triage-factory/issues/23)) ([1190c16](https://github.com/sky-ai-eng/triage-factory/commit/1190c16d594a65eadbc19a5d9437408948c09ff0))
* **seed:** default trigger for auto PR review on review-requested ([8473cb3](https://github.com/sky-ai-eng/triage-factory/commit/8473cb3ff9771b1a605a7a20d898e7b182174b8e))
* **seed:** self-review loop prompts + starter triggers (SKY-160) ([#35](https://github.com/sky-ai-eng/triage-factory/issues/35)) ([20d8078](https://github.com/sky-ai-eng/triage-factory/commit/20d80786c25ce5393e9b9efb6fb09671f4e9aaed))
* **settings:** expose auto-delegate toggle ([35a2827](https://github.com/sky-ai-eng/triage-factory/commit/35a28278677437226f5338d4595a05ee3a24d1c6))
* **setup:** Jira carry-over step (SKY-191) ([#31](https://github.com/sky-ai-eng/triage-factory/issues/31)) ([6c18c1b](https://github.com/sky-ai-eng/triage-factory/commit/6c18c1b44c58efb68a9a1da1e39a7171a6f72dec))
* **task_rules:** backend CRUD API + frontend Triage page ([#24](https://github.com/sky-ai-eng/triage-factory/issues/24)) ([d5a3075](https://github.com/sky-ai-eng/triage-factory/commit/d5a30750bfc7b0ceb28cb528bc690b15adc1be78))
* toast notification system + Tier 1/2 consumers (SKY-187) ([698a6af](https://github.com/sky-ai-eng/triage-factory/commit/698a6af24a9edf1e036352d8da540e72fe4919e9))
* toast notification system + Tier 1/2 consumers (SKY-187) ([5f452ee](https://github.com/sky-ai-eng/triage-factory/commit/5f452ee8ca9d17b681018143ee8d798bdc46c4ff))
* **tracker:** backfill pr:review_requested on initial GH discovery ([c59cec2](https://github.com/sky-ai-eng/triage-factory/commit/c59cec2d1613d4bb9cc18b24def7949fb931506c))
* **triage:** adjust jira poller behavior and update state ([0d3c9fb](https://github.com/sky-ai-eng/triage-factory/commit/0d3c9fba5046b09caaaac841d5f2075df763e89b))
* **triage:** filter events ([419f17a](https://github.com/sky-ai-eng/triage-factory/commit/419f17a4b3f55b724c26968cc8107143a58bfdb7))
* **triage:** restructured things around a core event primitive ([cf050c5](https://github.com/sky-ai-eng/triage-factory/commit/cf050c55c2032e12f462ca5bf83fe96cc2a6b149))
* **triggers:** add PUT /api/triggers/{id} for config edits ([391024e](https://github.com/sky-ai-eng/triage-factory/commit/391024e2f642e87d266021cb44e526b76dd6bbca))


### Bug Fixes

* **ai:** unstick tasks in failed scoring batches ([2e83eae](https://github.com/sky-ai-eng/triage-factory/commit/2e83eaec2435bee15bbaaea121e3575ad6cc011c))
* **board:** show agent cards in done column too ([4cc77e7](https://github.com/sky-ai-eng/triage-factory/commit/4cc77e76320d3e3a3f246ae8d51647824141ac90))
* bug fixes ([9dd2aa4](https://github.com/sky-ai-eng/triage-factory/commit/9dd2aa446801f04074d2d9b9d3ff05abd9cac331))
* **dashboard:** patch PR snapshot after draft toggle (SKY-150) ([#36](https://github.com/sky-ai-eng/triage-factory/issues/36)) ([eb45de8](https://github.com/sky-ai-eng/triage-factory/commit/eb45de8a91506a4d2f38e1fda77999e54922bf55))
* **dashboard:** scope PRs page to user-authored PRs only ([#30](https://github.com/sky-ai-eng/triage-factory/issues/30)) ([4610b9c](https://github.com/sky-ai-eng/triage-factory/commit/4610b9c38dd6ea6b2f56e411cf8bd4bdc77c17a2))
* **db:** standardize columns ([8a9bbbd](https://github.com/sky-ai-eng/triage-factory/commit/8a9bbbdfae9a54db035211614481975485c94ddb))
* **db:** stray paren in taskColumnsWithEntity broke all task queries ([a798796](https://github.com/sky-ai-eng/triage-factory/commit/a798796f520c6c60fd951fe8b6332eabe6839c18))
* **delegate:** clean up ghost ~/.claude/projects entries after delegated runs ([ba1df6b](https://github.com/sky-ai-eng/triage-factory/commit/ba1df6bf74e38da8d24bd9bbf08a3ede2d48af9e))
* **delegate:** curated Bash allowlist + scratch-dir guidance (SKY-194) ([#41](https://github.com/sky-ai-eng/triage-factory/issues/41)) ([ce91584](https://github.com/sky-ai-eng/triage-factory/commit/ce91584c18d05df37620caddc88e5840cb2b947b))
* **delegate:** events trigger at the right times now ([37e9e51](https://github.com/sky-ai-eng/triage-factory/commit/37e9e51f35a8d9b0d3f98296cedbd73a5070c8f3))
* **delegate:** improve PR reviewer tone ([2c5a9a4](https://github.com/sky-ai-eng/triage-factory/commit/2c5a9a469b9b6acf5203adcd4bfe44b6e2bcaceb))
* **delegate:** update the spawner's credentials in place to preserve cancel list ([d29d832](https://github.com/sky-ai-eng/triage-factory/commit/d29d832c3b30f4dd38bb58acee62580fc7820738))
* **docs:** update docs (again) with future target state ([fa55a31](https://github.com/sky-ai-eng/triage-factory/commit/fa55a319ed5fa54aab0a443b052887fe144f035f))
* **events:** align Event struct, scope jira rule, rename breaker_threshold ([#22](https://github.com/sky-ai-eng/triage-factory/issues/22)) ([7ab087d](https://github.com/sky-ai-eng/triage-factory/commit/7ab087d7dc99cfc343d3ae75e9ed5de2e874c657))
* **factory:** scan NULL-able task text columns via NullString in active-runs query ([#44](https://github.com/sky-ai-eng/triage-factory/issues/44)) ([bb41bce](https://github.com/sky-ai-eng/triage-factory/commit/bb41bce1b26d321ba89c156e38209c0edb64a3cf))
* **frontend:** strip legacy fields, align with entity model (SKY-185) ([#25](https://github.com/sky-ai-eng/triage-factory/issues/25)) ([b05b6c7](https://github.com/sky-ai-eng/triage-factory/commit/b05b6c7425cb99e0030ba8e23b9ab2546a275293))
* **frontend:** UX + QOL ([be41566](https://github.com/sky-ai-eng/triage-factory/commit/be41566974ff178889c06d1af6e13f87b5c009b8))
* GitHub poller hitting node + runtime limits on discovery ([#14](https://github.com/sky-ai-eng/triage-factory/issues/14)) ([4025731](https://github.com/sky-ai-eng/triage-factory/commit/4025731b6876b77aca5a7ad18fbd59e2175ce537))
* **github:** fetch and worktree optimizations ([e697ea2](https://github.com/sky-ai-eng/triage-factory/commit/e697ea23ee0f11bcc40b3afc797174c198179590))
* **github:** isLocalID was checking if the first character was a digit ([57e9286](https://github.com/sky-ai-eng/triage-factory/commit/57e9286ca8f3effc3d450a1feb4363034b681fbe))
* **jira:** send correct field to unassign ([3d7adde](https://github.com/sky-ai-eng/triage-factory/commit/3d7adde7e51d69561f0181e163466d5de40615bc))
* more bug fixes ([a0a0a3b](https://github.com/sky-ai-eng/triage-factory/commit/a0a0a3b8614839ece69c7846379bfa7d64f17def))
* PR dashboard, profiling TTL, and closed PR event type ([#8](https://github.com/sky-ai-eng/triage-factory/issues/8)) ([98f05eb](https://github.com/sky-ai-eng/triage-factory/commit/98f05eb0b48fe8c5ff8155e69ecbda93b7613df1))
* **prompts:** default by exact event name, not prefix ([79390b4](https://github.com/sky-ai-eng/triage-factory/commit/79390b41e63ec0c626d77072e199b26c1e7943a7))
* **repos:** cancel stale BranchPicker fetches with AbortController ([785b933](https://github.com/sky-ai-eng/triage-factory/commit/785b93319496a1614a39373f61dea43985934e95))
* **repos:** classification step improvement ([ab1f52b](https://github.com/sky-ai-eng/triage-factory/commit/ab1f52b8bb82f149821f4833db4fd8d69345cacb))
* **repos:** clean up BranchPicker debounce + fetch on unmount ([48c7f83](https://github.com/sky-ai-eng/triage-factory/commit/48c7f830a401433596d01c91f8a2983b64462616))
* **repos:** derive doc chip URLs from configured GitHub base URL ([37e64da](https://github.com/sky-ai-eng/triage-factory/commit/37e64daa230c92fca69d9fd8353cf746711d97fc))
* **repos:** highlight the effective branch, not raw base_branch ([c690881](https://github.com/sky-ai-eng/triage-factory/commit/c690881e6c6ad1ba2ff1750d9c8d9d3837d1c555))
* **repos:** the stored GitHub base URL is already the web root ([4788031](https://github.com/sky-ai-eng/triage-factory/commit/4788031a5cee62c5b0087399ee6e52f7e05c5c57))
* **review:** adapter to unwrap refractor.highlight(...).children ([95b06e4](https://github.com/sky-ai-eng/triage-factory/commit/95b06e4de7ffef154aaa429845e67d9e055ed919))
* **review:** darken diff colors ([b54b7b8](https://github.com/sky-ai-eng/triage-factory/commit/b54b7b8de2031a358ee4f0cf7e9638133797c06b))
* **review:** diff highlighting CSS classes ([29edbe5](https://github.com/sky-ai-eng/triage-factory/commit/29edbe568a4137d6895b9a2049b731bcaa372ddf))
* **review:** reformat existing diff view styling ([20cf4e2](https://github.com/sky-ai-eng/triage-factory/commit/20cf4e2f4588820b314d4aeae8230a0d9c420f3e))
* **review:** refractor imports ([cfa90f8](https://github.com/sky-ai-eng/triage-factory/commit/cfa90f81e9bbf9112387e45c4892c79609319ebd))
* **review:** use actual - not computed - cost for review body ([d0d71fd](https://github.com/sky-ai-eng/triage-factory/commit/d0d71fdf929a366e944d30130b4bd4eb819f6fce))
* **setup:** rewrite integrations step as multi-screen Jira flow (SKY-188) ([#29](https://github.com/sky-ai-eng/triage-factory/issues/29)) ([5098a8a](https://github.com/sky-ai-eng/triage-factory/commit/5098a8a5f4845d64c20deef80ec4f03f14be6961))
* **stock:** apply SKY-173 subtask gate to carry-over ([0982cb3](https://github.com/sky-ai-eng/triage-factory/commit/0982cb32240ce43c84c58659f4d12aab928efc3a))
* **TaskRulesPanel:** long rule names overflow the 340px panel ([150ca49](https://github.com/sky-ai-eng/triage-factory/commit/150ca49b57dc997c260083dd2f51b878108ebbea))
* **toast:** handle typed-nil hubs without panicking ([c0a8ed8](https://github.com/sky-ai-eng/triage-factory/commit/c0a8ed8aa7e9cc4bac455180001d2e03d8c7a9ed))
* **toast:** report exact skipped-task count from failed scoring batches ([a3d49d0](https://github.com/sky-ai-eng/triage-factory/commit/a3d49d0c2db8693998243cae570b2293a17cd87c))
* **toast:** throttle poll-failure toasts per source ([c4e41ec](https://github.com/sky-ai-eng/triage-factory/commit/c4e41ec191a41947054c61a37d1016626d5a4215))
* **tracker:** bump reviewRequests cap from 10 to 100 ([#40](https://github.com/sky-ai-eng/triage-factory/issues/40)) ([019ad90](https://github.com/sky-ai-eng/triage-factory/commit/019ad90552b74cb5d3e6cc016a006d259346494c))
* **tracker:** stamp PR review-request backfill with PR.CreatedAt ([19e1ea1](https://github.com/sky-ai-eng/triage-factory/commit/19e1ea156cb1b4eca44c7bdd4ab10f3eeb3bd73a))
