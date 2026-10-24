// The voice recorder's vocabulary, as the widgets and the store share it. Like
// attach.dart it imports nothing from `package:catenary_client`
// (test/store_boundary_test.dart).

/// THE GATE (CANT-247 ruling 0 → B). Voice sending is off: REC is drawn
/// disabled in the disabled color and no entry is ever composed from it.
///
/// WHY. A voice upload succeeds, and the message then cannot be sent: the
/// server's resolver admits a `voice` row only once it has `peaks` and
/// `duration_ms`, and nothing fills them until CANT-56 computes them
/// server-side. With this on, every voice note would end `failed` after a
/// second blob has been uploaded, and the client would be offering a control
/// the server cannot keep (Invariant 3).
///
/// THE RECORDER BEHIND IT IS BUILT AND TESTED: `AppStore.startRecording` and
/// `sendRecording`, over the platform recorder, compose a `voice` attachment
/// with the recorder's own `durationMs` and no peaks, and the Dart driver's
/// live test proves the real uploader gets a handle for one. CANT-56 flips
/// this constant in the same PR that makes peaks exist and adds the
/// end-to-end proof. It is the only line that decides it.
const bool voiceSendEnabled = false;

/// A finished recording: where the platform recorder left the file (as it
/// recorded it: no re-encode), and its own clock's length. There is no peaks
/// field and there must not be one: the server computes them once, because the
/// seeded generator overflows 2^53 and gives different bars in JavaScript than
/// in Dart.
class RecordedVoice {
  const RecordedVoice({required this.path, required this.durationMs});

  final String path;
  final int durationMs;
}

/// The platform recorder, one recording at a time.
abstract interface class VoiceRecorder {
  /// Starts recording. False when the microphone permission was refused;
  /// nothing is recording then. Throws when the platform cannot record.
  Future<bool> start();

  /// Ends the recording and returns it, or null when nothing was recorded.
  Future<RecordedVoice?> stop();

  /// Ends the recording and throws it away.
  Future<void> cancel();

  Future<void> dispose();
}

/// What asking to record came to.
sealed class RecordStart {
  const RecordStart();
}

final class RecordingStarted extends RecordStart {
  const RecordingStarted();
}

/// Recording did not start; [message] is shown beside the composer.
final class RecordRefused extends RecordStart {
  const RecordRefused(this.message);

  final String message;
}

const microphoneDenied = 'Microphone access is off. Allow it for Catenary in Settings to record.';
const recorderUnavailable = "Couldn't start the recorder.";
