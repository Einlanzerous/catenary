// The conversation a reader has open, kept across a cold start (CANT-370).
//
// The web carries it in the URL (CANT-369); a device has no URL, so it is one
// small file beside the address, `open-conversation`, holding the account's
// user id and the conversation id. KEYED BY ACCOUNT: a device re-enrolled as
// someone else never opens the previous person's thread. It is a hint and
// nothing reads it as a fact: a missing, unreadable or foreign file is "none
// open", and the id is only acted on while the conversation list holds it.

import 'dart:io';

const openConversationFileName = 'open-conversation';

File _file(String directory) => File('$directory${directory.endsWith('/') ? '' : '/'}$openConversationFileName');

/// The conversation [userId] had open, or null: no file, an empty or
/// malformed one, or one written for another account.
String? readOpenConversation(String directory, String userId) {
  try {
    final f = _file(directory);
    if (!f.existsSync()) return null;
    final parts = f.readAsStringSync().trim().split(' ');
    if (parts.length != 2 || parts[0] != userId || parts[1].isEmpty) return null;
    return parts[1];
  } on FileSystemException {
    return null;
  }
}

/// Records [conversationId] as open for [userId], or none open when it is
/// null. Through a temporary file and a rename, as the address is, so a kill
/// mid-write leaves the old value or the new one. A write that fails is
/// dropped: at worst the next cold start lands on the rail, as it did before.
void writeOpenConversation(String directory, String userId, String? conversationId) {
  try {
    final target = _file(directory);
    if (conversationId == null) {
      if (target.existsSync()) target.deleteSync();
      return;
    }
    final tmp = File('${target.path}.tmp')..writeAsStringSync('$userId $conversationId\n', flush: true);
    tmp.renameSync(target.path);
  } on FileSystemException {
    // A hint, not a record.
  }
}
