import express from 'express';

const app = express();
const port = process.env.PORT || 60000;

app.get('/', (req, res) => res.send("Hello, World!"))

app.listen(port, () => console.log(`example app listening on port ${port}!`));
