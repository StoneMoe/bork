import { render } from "solid-js/web";
import App from "./App";
import "./app.css";

const root = document.querySelector<HTMLDivElement>("#root");
if (!root) throw new Error("missing Solid root element");

render(() => <App />, root);
