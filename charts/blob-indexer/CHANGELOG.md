# Changelog

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
