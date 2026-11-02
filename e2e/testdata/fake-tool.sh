#!/bin/sh
# Stands in for a real tool such as a TTS engine in end-to-end tests.
# usage: fake-tool.sh MODE OUTPUT_PATH STEPS
mode=$1
out=$2
steps=$3
text=$(cat)

echo "$$" >"$MARKER_DIR/pid"
echo "run node=$TOOL_NODE" >>"$MARKER_DIR/runs"

case "$mode" in
fail)
	echo "the engine exploded" >&2
	exit 3
	;;
symlink)
	ln -s "$MARKER_DIR/secret" "$out"
	exit 0
	;;
env)
	env >"$out"
	exit 0
	;;
grandchild)
	# A long-lived child in the same process group. Stopping the job must kill it.
	sleep 300 &
	echo "$!" >"$MARKER_DIR/grandchild"
	wait
	exit 0
	;;
esac

i=1
while [ "$i" -le "$steps" ]; do
	echo "progress: $((i * 100 / steps))%"
	sleep 0.25
	i=$((i + 1))
done
printf 'node=%s text=%s' "$TOOL_NODE" "$text" >"$out"
