"""Watch facts arrive live. Run this, then run 01 or 03 in ANOTHER
terminal and watch the events appear here. Ctrl+C to stop."""
from centauri import Centauri

db = Centauri()  # connect to the already-running server

print("Watching for new facts... (Ctrl+C to stop)")
for event in db.watch():
    print(f"  NEW: {event.subject}  {event.value}  (facet={event.facet})")
