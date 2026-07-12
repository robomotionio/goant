function check(a,b){if(a!==b)throw a+" !== "+b;} var a=[1];check(a.unshift(0),2);check(a.length,2);console.log('PASS');
