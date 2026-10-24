import "./style.css";
import { finish } from "./signin.js";

// The page the group's login comes back to.
const msg = document.getElementById("msg");
finish()
  .then((back) => location.replace(back))
  .catch((e) => {
    msg.textContent = e.message;
    document.getElementById("again").hidden = false;
  });
