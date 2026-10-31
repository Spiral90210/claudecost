#!/bin/sh
# <swiftbar.hideAbout>true</swiftbar.hideAbout>
# <swiftbar.hideRunInTerminal>true</swiftbar.hideRunInTerminal>
# <swiftbar.hideDisablePlugin>true</swiftbar.hideDisablePlugin>
#
# claudecost menu bar plugin. The ".1m." in the filename sets the refresh interval.
# SwiftBar runs with a minimal PATH, so the binary is referenced by absolute path.
# The install steps in README.md rewrite the BIN line to your Go bin directory.

BIN="$HOME/go/bin/claudecost"

if [ ! -x "$BIN" ]; then
  echo "claudecost ? | sfimage=exclamationmark.triangle"
  echo "---"
  echo "claudecost not found at $BIN"
  echo "See the menu bar install steps in the claudecost README"
  exit 0
fi

# Add --db PATH here if your database is not at ~/.claude/cost.db.
exec "$BIN" summary --format swiftbar
