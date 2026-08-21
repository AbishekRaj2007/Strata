package manifest

import "errors"

// ErrCorrupt is the class every manifest-damage error wraps. docs/format.md §4
// admits no forward-compatibility provision in version 1, so an unrecognised
// edit type, an inconsistent file range, or a DELETE_FILE naming an absent
// file are all damage rather than something to skip: continuing past any of
// them would reconstruct a version that does not describe what is on disk.
var ErrCorrupt = errors.New("manifest corruption")
