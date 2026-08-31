/// Catenary wire protocol — the Dart half of the R4 (IDEA-27) contract.
///
/// Every type in here is generated from `schema/catenary.wire.v1.schema.json`
/// by `schema/codegen/generate.mjs`, the same walk that produces the
/// TypeScript for the Vue client and the Go for the server. Do not hand-write
/// a wire type in the Flutter client: that is the drift R4 exists to prevent,
/// and `npm run gen:check` in CI is what stops it happening quietly.
library catenary_wire;

export 'src/generated.dart';
