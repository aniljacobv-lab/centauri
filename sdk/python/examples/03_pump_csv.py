"""Bulk-load a CSV in one line. Creates its own sample file first."""
from pathlib import Path

from centauri import Centauri

csv_file = Path("sample_prices.csv")
csv_file.write_text(
    "subject,price_cents,kind\n"
    "toy:car,300,REGULAR\n"
    "toy:boat,550,REGULAR\n"
    "toy:kite,120,SALE\n",
    encoding="utf-8",
)

db = Centauri.launch(data="tutorial.log")
count = db.pump(csv_file)
print(f"Pumped {count} facts!")
for subject in ("toy:car", "toy:boat", "toy:kite"):
    print(subject, "->", [e.value for e in db.get(subject)])
db.stop()
