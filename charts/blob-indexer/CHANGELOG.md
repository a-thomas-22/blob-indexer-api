# Changelog

## [0.6.12](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-chart-v0.6.11...blob-indexer-chart-v0.6.12) (2026-08-02)


### Bug Fixes

* **helm:** update chart app version to 0.14.0 ([8d6969b](https://github.com/a-thomas-22/blob-indexer-api/commit/8d6969b76405430d42c54e5969d8fe56d8df06f1))

## [0.6.11](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-chart-v0.6.10...blob-indexer-chart-v0.6.11) (2026-07-14)


### Bug Fixes

* **helm:** update chart app version to 0.13.0 ([8feed81](https://github.com/a-thomas-22/blob-indexer-api/commit/8feed81ede2a21e67d1e8f19c44da539d87f7b0f))

## [0.6.10](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-chart-v0.6.9...blob-indexer-chart-v0.6.10) (2026-07-13)


### Bug Fixes

* **attribution:** update blob-list source URL and treat 404 as no-op ([#297](https://github.com/a-thomas-22/blob-indexer-api/issues/297)) ([c10618a](https://github.com/a-thomas-22/blob-indexer-api/commit/c10618a2ce71f8bdae89237eaad21ec36ba1b1c7))
* **helm:** update chart app version to 0.12.1 ([739accf](https://github.com/a-thomas-22/blob-indexer-api/commit/739accfba333bc5faa90a423e3001627fee3c682))

## [0.6.9](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-chart-v0.6.8...blob-indexer-chart-v0.6.9) (2026-07-13)


### Bug Fixes

* **helm:** update chart app version to 0.12.0 ([62eb3f3](https://github.com/a-thomas-22/blob-indexer-api/commit/62eb3f3f5288ea893c656c14366fd69782637f0a))

## [0.6.8](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-chart-v0.6.7...blob-indexer-chart-v0.6.8) (2026-07-06)


### Bug Fixes

* **helm:** render all indexer config keys in the ConfigMap ([#291](https://github.com/a-thomas-22/blob-indexer-api/issues/291)) ([888499a](https://github.com/a-thomas-22/blob-indexer-api/commit/888499a345dd23f87526c0a3f17ab4acfb7cc39c))
* **helm:** update chart app version to 0.11.0 ([0de26c6](https://github.com/a-thomas-22/blob-indexer-api/commit/0de26c607d76aea8253d3f7ed3ca78fb604c5102))

## [0.6.7](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-chart-v0.6.6...blob-indexer-chart-v0.6.7) (2026-07-05)


### Bug Fixes

* **helm:** update chart app version to 0.10.0 ([3cc2b88](https://github.com/a-thomas-22/blob-indexer-api/commit/3cc2b884691700247a04f9025299d975e3f423f0))

## [0.6.6](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-chart-v0.6.5...blob-indexer-chart-v0.6.6) (2026-07-05)


### Bug Fixes

* **api:** address [#239](https://github.com/a-thomas-22/blob-indexer-api/issues/239)/[#250](https://github.com/a-thomas-22/blob-indexer-api/issues/250) review comments + Cloudflare-aware WS client IP ([#251](https://github.com/a-thomas-22/blob-indexer-api/issues/251)) ([fc67285](https://github.com/a-thomas-22/blob-indexer-api/commit/fc67285ca839f8767de6ebe2cbd0c6dbdeaf6706))
* **helm:** source RPC URLs from a Secret and add liveness probes ([#240](https://github.com/a-thomas-22/blob-indexer-api/issues/240)) ([a484db1](https://github.com/a-thomas-22/blob-indexer-api/commit/a484db1b575b65dce4a45eb3bf217efd1deaa7da))
* **helm:** update chart app version to 0.9.0 ([04608a4](https://github.com/a-thomas-22/blob-indexer-api/commit/04608a43a381617690f19f1db94efca8f86e5095))


### Performance Improvements

* heavy caching for blob-flow views + delta-based write-path triggers ([#272](https://github.com/a-thomas-22/blob-indexer-api/issues/272)) ([5edd091](https://github.com/a-thomas-22/blob-indexer-api/commit/5edd091675409e5f5153bfd09b207b179acd3bf1))

## [0.6.5](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-chart-v0.6.4...blob-indexer-chart-v0.6.5) (2026-06-14)


### Bug Fixes

* **helm:** update chart app version to 0.8.1 ([3534ac3](https://github.com/a-thomas-22/blob-indexer-api/commit/3534ac34a77f493ef38fb91a8fdbf193180173fa))

## [0.6.4](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-chart-v0.6.3...blob-indexer-chart-v0.6.4) (2026-06-11)


### Bug Fixes

* auto-recover dirty schema and stop Argo CD killing running migrations ([#225](https://github.com/a-thomas-22/blob-indexer-api/issues/225)) ([5053b81](https://github.com/a-thomas-22/blob-indexer-api/commit/5053b8141cc947d29186702224079652a8b65455))
* **helm:** run the indexer as a StatefulSet for single-writer rollouts ([#226](https://github.com/a-thomas-22/blob-indexer-api/issues/226)) ([b995a62](https://github.com/a-thomas-22/blob-indexer-api/commit/b995a62039fe00398aa07ccd641bf91693032acb))
* **helm:** update chart app version to 0.8.0 ([#230](https://github.com/a-thomas-22/blob-indexer-api/issues/230)) ([de7dce8](https://github.com/a-thomas-22/blob-indexer-api/commit/de7dce8abb83a0626f1e16c4d14ce0e5a254d225))

## [0.6.3](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-chart-v0.6.2...blob-indexer-chart-v0.6.3) (2026-06-11)


### Bug Fixes

* **helm:** update chart for app version 0.7.2 ([#218](https://github.com/a-thomas-22/blob-indexer-api/issues/218)) ([fc4925f](https://github.com/a-thomas-22/blob-indexer-api/commit/fc4925fbdf51cddcd392a19ba4c1accab36d75b1))

## [0.6.2](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-chart-v0.6.1...blob-indexer-chart-v0.6.2) (2026-05-26)


### Bug Fixes

* **helm:** update chart app version to 0.7.1 ([#210](https://github.com/a-thomas-22/blob-indexer-api/issues/210)) ([d3cb8d2](https://github.com/a-thomas-22/blob-indexer-api/commit/d3cb8d200a306cec057b1fa3c6035482fab2bebc))

## [0.6.1](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-chart-v0.6.0...blob-indexer-chart-v0.6.1) (2026-05-26)


### Bug Fixes

* reduce dashboard database load ([#202](https://github.com/a-thomas-22/blob-indexer-api/issues/202)) ([93ddc67](https://github.com/a-thomas-22/blob-indexer-api/commit/93ddc67e207c520270db4854cf3cc0c06399d197))

## [0.6.0](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-chart-v0.5.1...blob-indexer-chart-v0.6.0) (2026-05-25)


### Features

* add configurable API CORS ([#196](https://github.com/a-thomas-22/blob-indexer-api/issues/196)) ([570d5c6](https://github.com/a-thomas-22/blob-indexer-api/commit/570d5c660ac80a6e0d0401c79a8e9606a275c73d))

## [0.5.1](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-chart-v0.5.0...blob-indexer-chart-v0.5.1) (2026-05-25)


### Bug Fixes

* **helm:** update chart app version to 0.5.1 ([46fa332](https://github.com/a-thomas-22/blob-indexer-api/commit/46fa332eb7f902dc20d2d8d87708586ebf75db33))

## [0.5.0](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-chart-v0.4.0...blob-indexer-chart-v0.5.0) (2026-05-25)


### Features

* serve dev API on separate port ([#183](https://github.com/a-thomas-22/blob-indexer-api/issues/183)) ([2b0079d](https://github.com/a-thomas-22/blob-indexer-api/commit/2b0079d1d0f430a1fa669c61d1476ceb26fccff9))

## [0.4.0](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-chart-v0.3.0...blob-indexer-chart-v0.4.0) (2026-05-25)


### Features

* load blob-list attributions dynamically ([#180](https://github.com/a-thomas-22/blob-indexer-api/issues/180)) ([1fb881a](https://github.com/a-thomas-22/blob-indexer-api/commit/1fb881ac66a5c0b4ee49889a45bea0786ffe42a4))


### Bug Fixes

* gate startup on database schema readiness ([#168](https://github.com/a-thomas-22/blob-indexer-api/issues/168)) ([787b17e](https://github.com/a-thomas-22/blob-indexer-api/commit/787b17e509a3c5dc1147b16fe7bfefa6e4556adc))
* **helm:** use existing DB secret for migrations ([#165](https://github.com/a-thomas-22/blob-indexer-api/issues/165)) ([41905d3](https://github.com/a-thomas-22/blob-indexer-api/commit/41905d3234214b955c50666a8ee98de0661547d1))

## [0.3.0](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-chart-v0.2.0...blob-indexer-chart-v0.3.0) (2026-05-24)


### Features

* add Helm chart integration tests with ct and kind ([#60](https://github.com/a-thomas-22/blob-indexer-api/issues/60)) ([d44e474](https://github.com/a-thomas-22/blob-indexer-api/commit/d44e4747af08a43d7cf444602465cc19f37832ae))
* split indexer and API into separate applications ([e0651e2](https://github.com/a-thomas-22/blob-indexer-api/commit/e0651e234f884369bb6cde3e63b5579c48911320))
* split indexer and API into separate applications ([7266f5c](https://github.com/a-thomas-22/blob-indexer-api/commit/7266f5c6eece2ffbac9e7677f5544535091fb408))


### Bug Fixes

* harden production readiness ([#154](https://github.com/a-thomas-22/blob-indexer-api/issues/154)) ([0e1b85a](https://github.com/a-thomas-22/blob-indexer-api/commit/0e1b85aaa48f76abbae0303bb17df7a2ffc24de0))
* **helm:** add restrictive pod and container security contexts ([#72](https://github.com/a-thomas-22/blob-indexer-api/issues/72)) ([c521b46](https://github.com/a-thomas-22/blob-indexer-api/commit/c521b466e666a3bbbf982e1f637e8140b6b51ee0))
* **helm:** add service account and database network policy ([#73](https://github.com/a-thomas-22/blob-indexer-api/issues/73)) ([ef84b18](https://github.com/a-thomas-22/blob-indexer-api/commit/ef84b182288517d472af4b5a931e462ebc4c4590))
* **helm:** stabilize chart CI install ([#157](https://github.com/a-thomas-22/blob-indexer-api/issues/157)) ([fd0bb18](https://github.com/a-thomas-22/blob-indexer-api/commit/fd0bb18afcbf7ea626bb682bf15da862443fc808))
* publish helm chart without bundled postgres ([#159](https://github.com/a-thomas-22/blob-indexer-api/issues/159)) ([d4b72f1](https://github.com/a-thomas-22/blob-indexer-api/commit/d4b72f1f1d157074549280d7131e267eb1f0cc1c))
* **security:** make DB sslmode configurable and enforce in non-dev ([#68](https://github.com/a-thomas-22/blob-indexer-api/issues/68)) ([27ef8aa](https://github.com/a-thomas-22/blob-indexer-api/commit/27ef8aae4fc75a7910aecd40fe483019c5d5f16a))
* **security:** move DB URL from ConfigMap to Secret ([#67](https://github.com/a-thomas-22/blob-indexer-api/issues/67)) ([6b92539](https://github.com/a-thomas-22/blob-indexer-api/commit/6b92539636a3c73630b99eb0928f424921239e95))

## [0.2.0](https://github.com/a-thomas-22/blob-indexer-api/compare/blob-indexer-chart-v0.1.0...blob-indexer-chart-v0.2.0) (2026-05-24)


### Features

* add Helm chart integration tests with ct and kind ([#60](https://github.com/a-thomas-22/blob-indexer-api/issues/60)) ([d44e474](https://github.com/a-thomas-22/blob-indexer-api/commit/d44e4747af08a43d7cf444602465cc19f37832ae))
* split indexer and API into separate applications ([e0651e2](https://github.com/a-thomas-22/blob-indexer-api/commit/e0651e234f884369bb6cde3e63b5579c48911320))
* split indexer and API into separate applications ([7266f5c](https://github.com/a-thomas-22/blob-indexer-api/commit/7266f5c6eece2ffbac9e7677f5544535091fb408))


### Bug Fixes

* **helm:** add restrictive pod and container security contexts ([#72](https://github.com/a-thomas-22/blob-indexer-api/issues/72)) ([c521b46](https://github.com/a-thomas-22/blob-indexer-api/commit/c521b466e666a3bbbf982e1f637e8140b6b51ee0))
* **helm:** add service account and database network policy ([#73](https://github.com/a-thomas-22/blob-indexer-api/issues/73)) ([ef84b18](https://github.com/a-thomas-22/blob-indexer-api/commit/ef84b182288517d472af4b5a931e462ebc4c4590))
* **security:** make DB sslmode configurable and enforce in non-dev ([#68](https://github.com/a-thomas-22/blob-indexer-api/issues/68)) ([27ef8aa](https://github.com/a-thomas-22/blob-indexer-api/commit/27ef8aae4fc75a7910aecd40fe483019c5d5f16a))
* **security:** move DB URL from ConfigMap to Secret ([#67](https://github.com/a-thomas-22/blob-indexer-api/issues/67)) ([6b92539](https://github.com/a-thomas-22/blob-indexer-api/commit/6b92539636a3c73630b99eb0928f424921239e95))
