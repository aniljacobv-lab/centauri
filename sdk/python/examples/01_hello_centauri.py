"""Hello Centauri: start, save a fact, read it back. (~10 seconds)"""
from centauri import Centauri

db = Centauri.launch(data="tutorial.log")

db.add("toy:robot", {"price_cents": 500, "color": "silver"})
db.add("toy:robot", {"price_cents": 450, "color": "silver"})  # update — old kept!

print("Current:", [e.value for e in db.get("toy:robot")])
print("History:", [e.value for e in db.history("toy:robot")])
print("Stats:  ", db.stats())

db.stop()
