#!/bin/sh
# Start command for an app image that carries the watcher. Put it next to
# the watcher binary and give it your app as arguments:
#
#   ENTRYPOINT ["/usr/local/bin/watcher-entrypoint.sh"]
#   CMD ["your-app", "--its", "arguments"]
#
# When the operator has set a config directory, the watch process is started
# in the background. Then this shell turns into the app, so the app is PID 1
# and the watch process is its child. If the watcher cannot start, the app
# starts anyway.

if [ -n "${CONFIG_RELOAD_DIR:-}" ]; then
  "${0%/*}/watcher" &
fi

exec "$@"
