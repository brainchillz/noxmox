# sanlock frozen on the holder (SIGSTOP). Expect: wdmd and the watchdog
# reset the node in under 130 s, before the peer can acquire anything.
. "$HERE/scenarios/_crash.sh"
freeze_sanlock() { log "freezing sanlock on $1"; s "$1" "pkill -STOP -x sanlock && echo frozen" 15; }
scenario_run() { crash_scenario sanlock-freeze freeze_sanlock 145 200; }
