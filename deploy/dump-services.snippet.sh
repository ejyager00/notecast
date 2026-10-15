# --- notecast --------------------------------------------------------------
# Add to ~/.config/containers/systemd/dump-services.sh after the Workout block,
# and add NC_DEST next to WK_DEST in both branches of the label `if`:
#   labelled:  NC_DEST="$DUMPS/pre-upgrade"
#   nightly:   NC_DEST="$DUMPS/notecast"
# plus "notecast" to the retention globs, mirroring workout.
NC_DB=/mnt/data/volumes/notecast/app.db
if [ -f "$NC_DB" ]; then
  mkdir -p "$NC_DEST"
  dest="$NC_DEST/notecast-${TS}${SUFFIX}.db"
  if sqlite3 "$NC_DB" ".backup '$dest'"; then
    if verify "$dest"; then log "notecast -> $dest ($(stat -c%s "$dest") bytes)"
    else rm -f "$dest"; rc=1; fi
  else
    warn "notecast: .backup failed"; rc=1
  fi
else
  log "notecast not deployed -- skipping"
fi
