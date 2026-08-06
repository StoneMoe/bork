import { render } from "solid-js/web";
import App from "./App";
import "./base.css";
import "./app.css";
import "./room.css";
import "./room-controls.css";
import "./settings.css";

const root = document.querySelector<HTMLDivElement>("#root");
if (!root) throw new Error("missing Solid root element");

render(() => <App />, root);
