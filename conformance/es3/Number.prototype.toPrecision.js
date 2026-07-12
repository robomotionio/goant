function check(a,b){if(a!==b)throw a+" !== "+b;} check((123.456).toPrecision(4),'123.5');check((0.0001234).toPrecision(2),'0.00012');check((123).toPrecision(5),'123.00');console.log('PASS');
