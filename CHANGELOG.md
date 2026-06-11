# Changelog

## [0.7.2](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-api-v0.7.1...blob-indexer-api-v0.7.2) (2026-06-11)


### Performance Improvements

* serve charts from pre-aggregated rollups and cache chart responses ([#216](https://github.com/a-thomas-22/blob-indexer-api/issues/216)) ([13a7271](https://github.com/a-thomas-22/blob-indexer-api/commit/13a7271bfcb0d7f7364e1f24b4e00bc5110732c1))


### Dependencies

* bump gitleaks/gitleaks-action from 2 to 3 ([#215](https://github.com/a-thomas-22/blob-indexer-api/issues/215)) ([ca30537](https://github.com/a-thomas-22/blob-indexer-api/commit/ca30537318f140b52384e6d42698bf86d6a2852f))
* bump golang.org/x/sync from 0.19.0 to 0.20.0 ([#214](https://github.com/a-thomas-22/blob-indexer-api/issues/214)) ([d32729a](https://github.com/a-thomas-22/blob-indexer-api/commit/d32729a7517d99d0f405ea59c7a6ece71d7aa1dc))

## [0.7.1](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-api-v0.7.0...blob-indexer-api-v0.7.1) (2026-05-26)


### Bug Fixes

* **stats:** remove full-history blob aggregate ([#209](https://github.com/a-thomas-22/blob-indexer-api/issues/209)) ([983a8a0](https://github.com/a-thomas-22/blob-indexer-api/commit/983a8a0b29776d7d8e61e400dc9b4ae28e0efb45))


### Performance Improvements

* add public API rollups ([#211](https://github.com/a-thomas-22/blob-indexer-api/issues/211)) ([4dfab44](https://github.com/a-thomas-22/blob-indexer-api/commit/4dfab446400e4f406e421e364b16b49922e31480))

## [0.7.0](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-api-v0.6.0...blob-indexer-api-v0.7.0) (2026-05-26)


### Features

* add chart API endpoints ([#207](https://github.com/a-thomas-22/blob-indexer-api/issues/207)) ([46a60c6](https://github.com/a-thomas-22/blob-indexer-api/commit/46a60c63ca951ea8ef69f127dc9ec2d33a9875ac))


### Bug Fixes

* clarify API cost units ([#201](https://github.com/a-thomas-22/blob-indexer-api/issues/201)) ([37f23f4](https://github.com/a-thomas-22/blob-indexer-api/commit/37f23f4788d174ba7b5d1085f664a522d69d78c3))
* include pricing in websocket new block events ([#206](https://github.com/a-thomas-22/blob-indexer-api/issues/206)) ([7459b9a](https://github.com/a-thomas-22/blob-indexer-api/commit/7459b9a6fa73739688a7753a3a5f69317904799e))
* **indexer:** store one row per blob instead of one row per blob tx ([#203](https://github.com/a-thomas-22/blob-indexer-api/issues/203)) ([2123d22](https://github.com/a-thomas-22/blob-indexer-api/commit/2123d22651214b685373b7c8be9d1ece5cb68a94))
* reduce dashboard database load ([#202](https://github.com/a-thomas-22/blob-indexer-api/issues/202)) ([93ddc67](https://github.com/a-thomas-22/blob-indexer-api/commit/93ddc67e207c520270db4854cf3cc0c06399d197))

## [0.6.0](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-api-v0.5.1...blob-indexer-api-v0.6.0) (2026-05-25)


### Features

* add configurable API CORS ([#196](https://github.com/a-thomas-22/blob-indexer-api/issues/196)) ([570d5c6](https://github.com/a-thomas-22/blob-indexer-api/commit/570d5c660ac80a6e0d0401c79a8e9606a275c73d))

## [0.5.1](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-api-v0.5.0...blob-indexer-api-v0.5.1) (2026-05-25)


### Bug Fixes

* **deps:** bump otel to v1.41.0 (CVE-2026-29181) ([#189](https://github.com/a-thomas-22/blob-indexer-api/issues/189)) ([86300c8](https://github.com/a-thomas-22/blob-indexer-api/commit/86300c80285f0adfb50cd1c6f21aae4a28389b63))
* resume indexer backfill from persisted cursor ([#191](https://github.com/a-thomas-22/blob-indexer-api/issues/191)) ([79a91a2](https://github.com/a-thomas-22/blob-indexer-api/commit/79a91a2e87fc306c3568e8fbfe16c30272501fb1))

## [0.5.0](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-api-v0.4.0...blob-indexer-api-v0.5.0) (2026-05-25)


### Features

* expose backfill activity in status ([#182](https://github.com/a-thomas-22/blob-indexer-api/issues/182)) ([c6fc2f8](https://github.com/a-thomas-22/blob-indexer-api/commit/c6fc2f8c902499d06acbbc9409bcf61549aa9b80))
* serve dev API on separate port ([#183](https://github.com/a-thomas-22/blob-indexer-api/issues/183)) ([2b0079d](https://github.com/a-thomas-22/blob-indexer-api/commit/2b0079d1d0f430a1fa669c61d1476ceb26fccff9))


### Bug Fixes

* quiet stale pending transaction lookups ([#184](https://github.com/a-thomas-22/blob-indexer-api/issues/184)) ([ec531b6](https://github.com/a-thomas-22/blob-indexer-api/commit/ec531b61c729ce2f8d072fbcb9f084f5e598a82d))

## [0.4.0](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-api-v0.3.0...blob-indexer-api-v0.4.0) (2026-05-25)


### Features

* add blob display helpers ([#177](https://github.com/a-thomas-22/blob-indexer-api/issues/177)) ([f54d701](https://github.com/a-thomas-22/blob-indexer-api/commit/f54d7011c28378ca191567c4711c020a32f0a22c))
* add blob market pressure indicators ([#171](https://github.com/a-thomas-22/blob-indexer-api/issues/171)) ([1542aaa](https://github.com/a-thomas-22/blob-indexer-api/commit/1542aaa213e17257ebdc196cc59a6a7f8b022453))
* add blob mempool pressure endpoint ([#172](https://github.com/a-thomas-22/blob-indexer-api/issues/172)) ([f6c9d12](https://github.com/a-thomas-22/blob-indexer-api/commit/f6c9d125880f2190cd070e9420f56108933b8904))
* add blob user breakdowns ([#175](https://github.com/a-thomas-22/blob-indexer-api/issues/175)) ([fcfab6a](https://github.com/a-thomas-22/blob-indexer-api/commit/fcfab6af87939841725ab2261f1ac9f6001b907d))
* add rolling blob market stats ([#173](https://github.com/a-thomas-22/blob-indexer-api/issues/173)) ([ed4246c](https://github.com/a-thomas-22/blob-indexer-api/commit/ed4246c69d0abbb3901154075590803b6f6e38cb))
* add top unattributed blob users endpoint ([#178](https://github.com/a-thomas-22/blob-indexer-api/issues/178)) ([35f7352](https://github.com/a-thomas-22/blob-indexer-api/commit/35f73528c9d216c514c42dccb7af44fa2f37f491))
* expose blob space occupancy ([#170](https://github.com/a-thomas-22/blob-indexer-api/issues/170)) ([17cebbd](https://github.com/a-thomas-22/blob-indexer-api/commit/17cebbd089192b9905e83df2085e1681c3ccc1ab))
* expose indexer freshness ([#176](https://github.com/a-thomas-22/blob-indexer-api/issues/176)) ([5855971](https://github.com/a-thomas-22/blob-indexer-api/commit/585597186269c0fcdf20469966796874155fb6b9))
* expose realized blob costs ([#169](https://github.com/a-thomas-22/blob-indexer-api/issues/169)) ([f30802a](https://github.com/a-thomas-22/blob-indexer-api/commit/f30802aac96d1df23a6fa0dcda323b191b25392f))
* load blob-list attributions dynamically ([#180](https://github.com/a-thomas-22/blob-indexer-api/issues/180)) ([1fb881a](https://github.com/a-thomas-22/blob-indexer-api/commit/1fb881ac66a5c0b4ee49889a45bea0786ffe42a4))


### Bug Fixes

* gate startup on database schema readiness ([#168](https://github.com/a-thomas-22/blob-indexer-api/issues/168)) ([787b17e](https://github.com/a-thomas-22/blob-indexer-api/commit/787b17e509a3c5dc1147b16fe7bfefa6e4556adc))
* **helm:** use existing DB secret for migrations ([#165](https://github.com/a-thomas-22/blob-indexer-api/issues/165)) ([41905d3](https://github.com/a-thomas-22/blob-indexer-api/commit/41905d3234214b955c50666a8ee98de0661547d1))
* keep failed block retries recoverable ([#166](https://github.com/a-thomas-22/blob-indexer-api/issues/166)) ([8656ac3](https://github.com/a-thomas-22/blob-indexer-api/commit/8656ac33abd04c35110cddf8b0fe4445841dab09))

## [0.3.0](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-api-v0.2.0...blob-indexer-api-v0.3.0) (2026-05-24)


### Features

* add blob-flow frontend to Tilt dev setup ([a63f80a](https://github.com/a-thomas-22/blob-indexer-api/commit/a63f80a066d74aeceabbd4d8614f5424a1ed6a2e))
* add blob-flow frontend to Tilt dev setup ([be736db](https://github.com/a-thomas-22/blob-indexer-api/commit/be736dbe9f3d42c0e7a659bc2db0c6c4e3dfeff6))
* add comprehensive blob pricing data and BPO fork support ([54c752f](https://github.com/a-thomas-22/blob-indexer-api/commit/54c752f726e8b96eb91a84d2d57f1b2045587d42))
* add comprehensive blob pricing data and BPO fork support ([2dea5ee](https://github.com/a-thomas-22/blob-indexer-api/commit/2dea5eecf057c2c7dec7721e5b792739d6ff0e0d))
* add Helm chart integration tests with ct and kind ([#60](https://github.com/a-thomas-22/blob-indexer-api/issues/60)) ([d44e474](https://github.com/a-thomas-22/blob-indexer-api/commit/d44e4747af08a43d7cf444602465cc19f37832ae))
* add HTTP 429 rate limit handling for RPC calls ([b815303](https://github.com/a-thomas-22/blob-indexer-api/commit/b815303027e4fa2876219649650d22470138814c))
* add HTTP 429 rate limit handling for RPC calls ([5a94c9b](https://github.com/a-thomas-22/blob-indexer-api/commit/5a94c9b87b3f065ba8869f04d5fab65fb8db8c89))
* add routes ([#128](https://github.com/a-thomas-22/blob-indexer-api/issues/128)) ([83a6770](https://github.com/a-thomas-22/blob-indexer-api/commit/83a677044e3ffe645647aa2ab2c4bf6b12d73c15))
* **api:** add WebSocket real-time blob feed ([#126](https://github.com/a-thomas-22/blob-indexer-api/issues/126)) ([758020d](https://github.com/a-thomas-22/blob-indexer-api/commit/758020de9b019734775203adcca2a260efd6f594))
* **api:** remove hardcoded mock payloads from dev endpoints ([#114](https://github.com/a-thomas-22/blob-indexer-api/issues/114)) ([22b2c14](https://github.com/a-thomas-22/blob-indexer-api/commit/22b2c14825bc646ec4c1098bbdb075749eceb3b6))
* make DB pool sizes and indexer constants configurable ([50303fb](https://github.com/a-thomas-22/blob-indexer-api/commit/50303fb96fc3d5a972bc602db7ef0fec25487442))
* split indexer and API into separate applications ([e0651e2](https://github.com/a-thomas-22/blob-indexer-api/commit/e0651e234f884369bb6cde3e63b5579c48911320))
* split indexer and API into separate applications ([7266f5c](https://github.com/a-thomas-22/blob-indexer-api/commit/7266f5c6eece2ffbac9e7677f5544535091fb408))
* upgrade to Go 1.26 and make govulncheck strict ([518651f](https://github.com/a-thomas-22/blob-indexer-api/commit/518651f9e28dcc9002dbde681c3b26f7b6dd4c01))


### Bug Fixes

* add request body size limit to prevent DoS ([8ca1098](https://github.com/a-thomas-22/blob-indexer-api/commit/8ca10985fc3d4a05f4416d02578162f4f8fd9f90))
* address lint issues and improve test coverage ([954212a](https://github.com/a-thomas-22/blob-indexer-api/commit/954212a5e27932167158dce5f477ebb8803a521f))
* **api:** add baseline security response headers ([#81](https://github.com/a-thomas-22/blob-indexer-api/issues/81)) ([71c01c2](https://github.com/a-thomas-22/blob-indexer-api/commit/71c01c28df78448f4ca957af0f32f4bab4d501b9))
* **api:** cache and bound expensive aggregation queries ([#79](https://github.com/a-thomas-22/blob-indexer-api/issues/79)) ([a80bd53](https://github.com/a-thomas-22/blob-indexer-api/commit/a80bd533b357cd43a073b3f7c5f6deab7910e22d))
* **api:** configure read, write, and idle server timeouts ([#89](https://github.com/a-thomas-22/blob-indexer-api/issues/89)) ([79cf0ce](https://github.com/a-thomas-22/blob-indexer-api/commit/79cf0ceb0a361d5ac83005e12a09e181acd596d0))
* **api:** stop leaking JSON encoder errors to clients ([#78](https://github.com/a-thomas-22/blob-indexer-api/issues/78)) ([08cb88a](https://github.com/a-thomas-22/blob-indexer-api/commit/08cb88a10f2f1aefc54c592c60b5650ac2399e28))
* **api:** validate txHash format before DB lookup ([#85](https://github.com/a-thomas-22/blob-indexer-api/issues/85)) ([8c3d533](https://github.com/a-thomas-22/blob-indexer-api/commit/8c3d53381c487aa64cdffb9852ac3d9a5912cf1b))
* **attribution:** synchronize knownUsers map access ([#83](https://github.com/a-thomas-22/blob-indexer-api/issues/83)) ([7c00bd8](https://github.com/a-thomas-22/blob-indexer-api/commit/7c00bd8e3b78d8ffbc60ed10983de17d1d016090))
* batch UpdateUserLastSeen into a single query to fix N+1 problem ([d128713](https://github.com/a-thomas-22/blob-indexer-api/commit/d128713c49670869c758c8b75e6d02d78c718905))
* **ci:** pin trivy action to a fixed version ([#82](https://github.com/a-thomas-22/blob-indexer-api/issues/82)) ([51989a1](https://github.com/a-thomas-22/blob-indexer-api/commit/51989a1d9aa8715492398800b51bcee1ad06e67b))
* **db:** wrap GetIndexedBlockHash errors with context ([#107](https://github.com/a-thomas-22/blob-indexer-api/issues/107)) ([d7951bb](https://github.com/a-thomas-22/blob-indexer-api/commit/d7951bb8b38a2e4657ace13212df85271b5f6b99))
* **docker:** remove bundled config.yaml from runtime image ([#80](https://github.com/a-thomas-22/blob-indexer-api/issues/80)) ([35cf866](https://github.com/a-thomas-22/blob-indexer-api/commit/35cf866e9603f11c357c8afa4f0ae94f60da05e0))
* **ethereum:** guard against missing latest header number ([#76](https://github.com/a-thomas-22/blob-indexer-api/issues/76)) ([77b673e](https://github.com/a-thomas-22/blob-indexer-api/commit/77b673e6146d9bbfcb6cade742e1ef7abc0d8fc4))
* **ethereum:** harden eth_blobBaseFee parsing ([#77](https://github.com/a-thomas-22/blob-indexer-api/issues/77)) ([e779397](https://github.com/a-thomas-22/blob-indexer-api/commit/e779397000673d3f057d0000f67c5572c384e424))
* gate all /dev/* endpoints behind dev mode middleware ([07c831e](https://github.com/a-thomas-22/blob-indexer-api/commit/07c831ea03d93107d8791867b27a7a2a9c785878))
* harden production readiness ([#154](https://github.com/a-thomas-22/blob-indexer-api/issues/154)) ([0e1b85a](https://github.com/a-thomas-22/blob-indexer-api/commit/0e1b85aaa48f76abbae0303bb17df7a2ffc24de0))
* **helm:** add restrictive pod and container security contexts ([#72](https://github.com/a-thomas-22/blob-indexer-api/issues/72)) ([c521b46](https://github.com/a-thomas-22/blob-indexer-api/commit/c521b466e666a3bbbf982e1f637e8140b6b51ee0))
* **helm:** add service account and database network policy ([#73](https://github.com/a-thomas-22/blob-indexer-api/issues/73)) ([ef84b18](https://github.com/a-thomas-22/blob-indexer-api/commit/ef84b182288517d472af4b5a931e462ebc4c4590))
* **helm:** stabilize chart CI install ([#157](https://github.com/a-thomas-22/blob-indexer-api/issues/157)) ([fd0bb18](https://github.com/a-thomas-22/blob-indexer-api/commit/fd0bb18afcbf7ea626bb682bf15da862443fc808))
* **indexer:** avoid global cancel on single network failure ([#110](https://github.com/a-thomas-22/blob-indexer-api/issues/110)) ([f29fe20](https://github.com/a-thomas-22/blob-indexer-api/commit/f29fe20d7375b9aad509a79429437bda65920747))
* **indexer:** clean up stale pending blobs and promote on confirmation ([#125](https://github.com/a-thomas-22/blob-indexer-api/issues/125)) ([08ab8d6](https://github.com/a-thomas-22/blob-indexer-api/commit/08ab8d64b6ab50b46941cc6ebc9a2dc1d8521fe7))
* **indexer:** evict permanently failed blocks from retry map ([#90](https://github.com/a-thomas-22/blob-indexer-api/issues/90)) ([bb8e8ed](https://github.com/a-thomas-22/blob-indexer-api/commit/bb8e8edc2eda440a98ff498499ccb26de9da3942))
* **indexer:** recover from panics in block worker loop ([#74](https://github.com/a-thomas-22/blob-indexer-api/issues/74)) ([7475623](https://github.com/a-thomas-22/blob-indexer-api/commit/74756238112f00384091032b1f94fc376b0a5768))
* **indexer:** stop 128x overcount in blob_size_bytes ([#96](https://github.com/a-thomas-22/blob-indexer-api/issues/96)) ([b994122](https://github.com/a-thomas-22/blob-indexer-api/commit/b994122ffc3356624e1694476959cb3bbce4cd6d))
* **indexer:** use context-driven worker shutdown without channel close ([#84](https://github.com/a-thomas-22/blob-indexer-api/issues/84)) ([1762a44](https://github.com/a-thomas-22/blob-indexer-api/commit/1762a44cfe66431cf135bbab24e1a9bac4da68e1))
* **indexer:** wrap reorg and reindex deletes in transactions ([#75](https://github.com/a-thomas-22/blob-indexer-api/issues/75)) ([d66dc0c](https://github.com/a-thomas-22/blob-indexer-api/commit/d66dc0c8a1b3ee404750bea6aaf66ad606757294))
* **logger:** honor configured log format in initialization ([#105](https://github.com/a-thomas-22/blob-indexer-api/issues/105)) ([81612df](https://github.com/a-thomas-22/blob-indexer-api/commit/81612dfad329b843b548aa4f0e41fcfce1a0e225))
* prevent in-flight query failures by correcting shutdown ordering ([fb2f36b](https://github.com/a-thomas-22/blob-indexer-api/commit/fb2f36b62c60b8059b4620bf4d0e134801ed3df7))
* publish helm chart without bundled postgres ([#159](https://github.com/a-thomas-22/blob-indexer-api/issues/159)) ([d4b72f1](https://github.com/a-thomas-22/blob-indexer-api/commit/d4b72f1f1d157074549280d7131e267eb1f0cc1c))
* remove Railway config and migrate golangci-lint config ([1c00abf](https://github.com/a-thomas-22/blob-indexer-api/commit/1c00abf9b6def70f3952f6b7046335fecb819d2b))
* rename max to maxBlobs to avoid builtin shadow lint error ([6620632](https://github.com/a-thomas-22/blob-indexer-api/commit/66206324652e80d587e68a43e41fbc52e3ed71c2))
* resolve CI failures in mod-tidy, govulncheck, and dependency-review ([bbfca86](https://github.com/a-thomas-22/blob-indexer-api/commit/bbfca86d7934b1c91e2bb05df70e26704548b86c))
* resolve lint errors and restore test coverage above 50% ([2307a86](https://github.com/a-thomas-22/blob-indexer-api/commit/2307a86d3d0e8e4ddfa23b38adae5220ab2fcaf7))
* restrict Tilt deploys to local kind-dev cluster ([#124](https://github.com/a-thomas-22/blob-indexer-api/issues/124)) ([c390c94](https://github.com/a-thomas-22/blob-indexer-api/commit/c390c94e9f20208f0813fa5103b20e203002c81b))
* **security:** avoid unbounded txpool_content mempool calls ([#71](https://github.com/a-thomas-22/blob-indexer-api/issues/71)) ([21af3fd](https://github.com/a-thomas-22/blob-indexer-api/commit/21af3fdbaa4dbc43c2d65889d133e2d0c934332c))
* **security:** make DB sslmode configurable and enforce in non-dev ([#68](https://github.com/a-thomas-22/blob-indexer-api/issues/68)) ([27ef8aa](https://github.com/a-thomas-22/blob-indexer-api/commit/27ef8aae4fc75a7910aecd40fe483019c5d5f16a))
* **security:** move DB URL from ConfigMap to Secret ([#67](https://github.com/a-thomas-22/blob-indexer-api/issues/67)) ([6b92539](https://github.com/a-thomas-22/blob-indexer-api/commit/6b92539636a3c73630b99eb0928f424921239e95))
* **security:** remove spoofable X-Real-Ip from rate limiter ([#69](https://github.com/a-thomas-22/blob-indexer-api/issues/69)) ([92be2f4](https://github.com/a-thomas-22/blob-indexer-api/commit/92be2f46bdf2dd7234976e9fa14dcc7a776bd0d8))
* **security:** require API key for dev endpoints ([#70](https://github.com/a-thomas-22/blob-indexer-api/issues/70)) ([2603af2](https://github.com/a-thomas-22/blob-indexer-api/commit/2603af2812c6563609aa49aba39c664dbaa77062))
* SQL injection vulnerability in DevDatabase handler ([48cf610](https://github.com/a-thomas-22/blob-indexer-api/commit/48cf6100cc5d849c04f08cc51ea176788a5a2bf4))
* **tilt:** fix broken dev setup and align with production config ([#127](https://github.com/a-thomas-22/blob-indexer-api/issues/127)) ([1b31fd0](https://github.com/a-thomas-22/blob-indexer-api/commit/1b31fd0649e51a04c618b8a39bd2e71dae388e27))
* update coverage tests for new insertBlockData signature and block_metrics ([2787fa0](https://github.com/a-thomas-22/blob-indexer-api/commit/2787fa0cca2d58b620cf6abdbd06953caf94cf2e))


### Dependencies

* bump github.com/go-chi/chi/v5 from 5.2.5 to 5.3.0 ([#155](https://github.com/a-thomas-22/blob-indexer-api/issues/155)) ([ff274a3](https://github.com/a-thomas-22/blob-indexer-api/commit/ff274a3c1aed641788492bb91bdb362ef3c2b591))

## [0.2.0](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-api-v0.1.0...blob-indexer-api-v0.2.0) (2026-03-09)


### Features

* add pagination offset support to API endpoints ([2b4ef9e](https://github.com/a-thomas-22/blob-indexer-api/commit/2b4ef9ec3db27ab85cd2a4d7f81d2adbfe48042b))
* **api:** add API versioning with /api/v1/ prefix ([f3957b3](https://github.com/a-thomas-22/blob-indexer-api/commit/f3957b3c94e1031ccbb28e4da57e11fa095abbdc))
* **api:** add Content-Type validation for POST/PUT/PATCH endpoints ([80940ad](https://github.com/a-thomas-22/blob-indexer-api/commit/80940addd24239fb0374bd7115da9b3e2b52b22a))


### Bug Fixes

* address remaining lint issues after CI-fix cherry-pick ([ae78a6a](https://github.com/a-thomas-22/blob-indexer-api/commit/ae78a6ad974f662405c01fff7da3fd5721758ab9))
* **api:** handle JSON encode error in content-type middleware ([687ac03](https://github.com/a-thomas-22/blob-indexer-api/commit/687ac031f3be30532a4efc3c301079179bb73271))
* apply goimports grouping in config imports ([9d5cb15](https://github.com/a-thomas-22/blob-indexer-api/commit/9d5cb1569459ce1e4fc9d7725a36d0a99045acd4))
* replace fmt.Printf with structured zap logger in config package ([39542ac](https://github.com/a-thomas-22/blob-indexer-api/commit/39542ac08c9ce1a62fda5d3e6d50ebbf1702a4d2))
