"""Time travel: ask what was true at any moment — and what you BELIEVED
at any moment. Run 01 first (or any data works)."""
from datetime import datetime, timedelta, timezone

from centauri import Centauri

db = Centauri.launch(data="tutorial.log")

now = datetime.now(timezone.utc)
yesterday = now - timedelta(days=1)

db.add("toy:rocket", {"price_cents": 900}, effective=yesterday)
db.add("toy:rocket", {"price_cents": 700})  # effective now

print("Now:        ", [e.value for e in db.get("toy:rocket")])
print("Yesterday:  ", [e.value for e in db.at("toy:rocket", yesterday)])
print("As known then (double time travel):",
      [e.value for e in db.at("toy:rocket", now, known=yesterday)])

db.stop()
