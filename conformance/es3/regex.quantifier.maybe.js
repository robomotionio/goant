function check(a,b){if(a!==b)throw a+" !== "+b;} check('color'.match(/colou?r/)[0],'color');check('colour'.match(/colou?r/)[0],'colour');console.log('PASS');
