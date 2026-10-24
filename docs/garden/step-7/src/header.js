import "./style.css";
import { signIn, signOut, who } from "./signin.js";

// Every page's header: "Sign in", or who is signed in and "Sign out".
const nav = document.querySelector("header nav");
if (nav) {
  const w = who();
  const a = document.createElement("a");
  a.href = "#";
  a.className = "who";
  a.textContent = w ? `${w.name} · Sign out` : "Sign in";
  a.title = w ? `Signed in with the group as ${w.email || w.name}` : "Sign in with the group's login";
  a.addEventListener("click", (e) => {
    e.preventDefault();
    w ? signOut() : signIn();
  });
  nav.append(a);
}
document.querySelectorAll("[data-who]").forEach((el) => {
  const w = who();
  el.hidden = el.dataset.who === "in" ? !w : !!w;
  el.querySelectorAll("[data-name]").forEach((n) => (n.textContent = w ? w.name : ""));
});
