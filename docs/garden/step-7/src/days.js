import { openDb, rows } from "./db.js";

openDb().then((db) => {
  for (const d of rows(db, "SELECT * FROM days")) {
    const li = document.createElement("li");
    li.textContent = `${d.day}: ${d.open}`;
    document.getElementById("days").append(li);
  }
});
